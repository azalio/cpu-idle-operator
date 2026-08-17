//go:build e2e

// Package e2e holds cpi-idle-operator's kind-based end-to-end tests.
//
// Every file here is gated behind the "e2e" build tag: `go build ./...`,
// `go vet ./...` and a plain `go test ./...` never compile this package, so
// none of it can accidentally become a merge-blocking requirement outside
// the dedicated e2e CI job (see .github/workflows/ci.yaml).
//
// These tests do not create or tear down the kind cluster themselves —
// that is deliberately left to the caller (CI or a developer), because it
// is infrastructure setup, not a test assertion, and kind-config.yaml is
// its own standalone artifact meant to be handed straight to `kind create
// cluster --config`. To run this suite locally:
//
//	KIND_EXPERIMENTAL_PROVIDER=podman kind create cluster \
//	    --name cpi-idle-e2e --config test/e2e/kind-config.yaml
//	podman build -t ghcr.io/azalio/cpi-idle-operator:latest .
//	podman save ghcr.io/azalio/cpi-idle-operator:latest -o /tmp/cpi-idle.tar
//	KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive \
//	    /tmp/cpi-idle.tar --name cpi-idle-e2e
//	go test -tags e2e -v ./test/e2e/...
//
// (`kind load docker-image` does not see podman-built images reliably —
// measured while writing this suite; `image-archive` does. On a native
// Docker host, `docker build` + `kind load docker-image` work directly and
// KIND_EXPERIMENTAL_PROVIDER is unset — this is the path CI uses.)
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

const (
	// clusterNameEnvVar overrides the kind cluster name this suite targets.
	clusterNameEnvVar = "CPI_E2E_KIND_CLUSTER"
	// clusterNameDefault matches the --name this repo's CI and the doc
	// comment above both use, so the common case needs no env var at all.
	clusterNameDefault = "cpi-idle-e2e"

	// prodCgroupRoot is exactly the --cgroup-root value
	// config/base/daemonset.yaml ships (its default, see internal/config's
	// defaultCgroupRoot). Every path this suite computes uses this root,
	// never a kind-specific workaround: the whole point of this suite is
	// proving whether production's own configuration works on kind, not
	// whether some other root would.
	prodCgroupRoot = "/sys/fs/cgroup"

	agentNamespace     = "cpi-idle-system"
	agentDaemonSet     = "cpi-idle-agent"
	agentLabelSelector = "app.kubernetes.io/component=agent"
	agentHealthPort    = 8081

	// podReadyTimeout matches hack/stand-probe.sh's own pod-wait timeout
	// (120s) for the same reason: a busybox image pull in CI can be slower
	// than on a warm local dev machine.
	podReadyTimeout = 120 * time.Second
	pollInterval    = 2 * time.Second
)

// clusterName returns the kind cluster name this suite targets.
func clusterName() string {
	if v := os.Getenv(clusterNameEnvVar); v != "" {
		return v
	}
	return clusterNameDefault
}

// nodeContainerName is the name kind gives a single-node cluster's
// control-plane container: "<cluster-name>-control-plane".
func nodeContainerName() string {
	return clusterName() + "-control-plane"
}

// containerRuntime returns the CLI this suite uses to exec into the kind
// node container: podman when KIND_EXPERIMENTAL_PROVIDER=podman is set
// (the only way to run kind on this project's macOS dev machines, whose
// Docker context is remote and does not support bind mounts), docker
// otherwise — including GitHub Actions' ubuntu runners, which ship a
// native Docker daemon and never set this variable.
func containerRuntime() string {
	if strings.EqualFold(os.Getenv("KIND_EXPERIMENTAL_PROVIDER"), "podman") {
		return "podman"
	}
	return "docker"
}

// kubectlArgs prefixes args with a --context flag pinned to this suite's
// kind cluster, so every kubectl call behaves identically regardless of
// the invoking shell's current context.
func kubectlArgs(args ...string) []string {
	return append([]string{"--context", "kind-" + clusterName()}, args...)
}

// runCmd runs name with args and fails t immediately, with combined
// output attached, on a non-zero exit. It is the shared plumbing for every
// blocking shell-out this suite performs.
func runCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// nodeExec runs args inside the kind node container via docker/podman exec
// and returns trimmed combined output plus the command's error.
//
// This is how every cgroup-file assertion in this suite reads state: never
// through a workload pod's own exec (the agent's runtime image is
// gcr.io/distroless/static — see Dockerfile — and has no shell to exec
// into, and even a pod that did have one would only show its own mount
// namespace, not what the node's kubelet actually manages), and never by
// trusting the agent's own logs (a log only says what the agent believes
// it did).
func nodeExec(args ...string) (string, error) {
	full := append([]string{"exec", nodeContainerName()}, args...)
	cmd := exec.Command(containerRuntime(), full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// requireNodeReachable fails t immediately, with a message distinct from
// any cgroup-view divergence, if the kind node container itself cannot be
// reached at all (wrong cluster name, container not running, wrong
// container runtime CLI). Without this check up front, a broken local
// setup and a real Open Question 1 divergence would look identical: both
// make nodePathExists return false, and a setup problem must never be
// misreported as "the cgroup views diverge".
func requireNodeReachable(t *testing.T) {
	t.Helper()
	if _, err := nodeExec("true"); err != nil {
		t.Fatalf("cannot exec into kind node container %q via %q: %v (is the cluster running? does %s / KIND_EXPERIMENTAL_PROVIDER match how it was created?)",
			nodeContainerName(), containerRuntime(), err, clusterNameEnvVar)
	}
}

// nodePathExists reports whether path exists as a directory inside the
// kind node container.
func nodePathExists(path string) bool {
	_, err := nodeExec("test", "-d", path)
	return err == nil
}

// readNodeFile reads path's content inside the kind node container.
func readNodeFile(path string) (string, error) {
	return nodeExec("cat", path)
}

// findNodePath searches the node's cgroup tree for any path containing
// needle, for diagnostics only — never for the pass/fail decision itself,
// which always goes through the agent's own PodCgroupPath computation.
func findNodePath(needle string) string {
	out, err := nodeExec("find", prodCgroupRoot, "-maxdepth", "8", "-iname", "*"+needle+"*")
	if err != nil || out == "" {
		return ""
	}
	return strings.SplitN(out, "\n", 2)[0]
}

// escapeUID mirrors kubelet's own escaping of a pod UID into a
// systemd-slice-safe name component (dashes to underscores). It
// intentionally duplicates internal/cgroup's private escaping logic in one
// line instead of importing it: this helper is only ever used to describe
// what the *real* kubelet produced, independently of the agent's own path
// computation under test.
func escapeUID(uid string) string {
	return strings.ReplaceAll(uid, "-", "_")
}

// computeAgentPath returns the pod-cgroup path the agent's own
// cgroup.PodCgroupPath computes for uid under prodCgroupRoot with the
// systemd driver. kind's node image always configures kubelet with the
// systemd driver (confirmed by reading /var/lib/kubelet/config.yaml on the
// node while writing this suite), so DriverSystemd is the only branch this
// suite ever needs.
func computeAgentPath(t *testing.T, qosClass cgroup.QoSClass, uid string) string {
	t.Helper()
	path, err := cgroup.PodCgroupPath(prodCgroupRoot, cgroup.DriverSystemd, qosClass, uid)
	if err != nil {
		t.Fatalf("PodCgroupPath(%q, systemd, %q, %q): %v", prodCgroupRoot, qosClass, uid, err)
	}
	return path
}

// kubeClient builds a clientset for the kind cluster's own kubeconfig
// context ("kind-<name>", written by `kind create cluster` itself). It
// fails t immediately if no such cluster is reachable: a test that
// silently no-ops without a cluster would be worse than one that fails
// loudly, since "no cluster" must never look like "passed".
func kubeClient(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + clusterName()}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("build kubeconfig for kind cluster %q: %v (create it first: kind create cluster --name %s --config test/e2e/kind-config.yaml)",
			clusterName(), err, clusterName())
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("build clientset for kind cluster %q: %v", clusterName(), err)
	}
	return clientset
}

// createTempNamespace creates a uniquely-named namespace for one test run.
func createTempNamespace(t *testing.T, ctx context.Context, clientset *kubernetes.Clientset, prefix string) string {
	t.Helper()
	ns := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	return ns
}

// deleteNamespace best-effort deletes ns. Cleanup must never fail a test
// that has already reported its own result, so errors are logged, not
// fatal — the same convention hack/stand-probe.sh's cleanup() follows.
func deleteNamespace(t *testing.T, clientset *kubernetes.Clientset, ns string) {
	t.Helper()
	if err := clientset.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}); err != nil {
		t.Logf("cleanup: delete namespace %s: %v", ns, err)
	}
}

// applyProbePod creates a Burstable busybox pod requesting cpuRequest CPU,
// with podAnnotations attached (nil for none). It targets no specific
// node: kind-config.yaml provisions exactly one node, so every pod this
// suite creates lands on the one node it inspects.
func applyProbePod(t *testing.T, ctx context.Context, clientset *kubernetes.Clientset, namespace, name, cpuRequest string, podAnnotations map[string]string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: podAnnotations},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "workload",
				Image:   "busybox:1.36",
				Command: []string{"sh", "-c", "while true; do sleep 3600; done"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(cpuRequest),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
			}},
		},
	}
	created, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s/%s: %v", namespace, name, err)
	}
	return created
}

// isPodReady reports whether pod's PodReady condition is True.
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isPodRunning reports whether pod's phase is Running — deliberately
// weaker than isPodReady: this suite's DaemonSet pod is expected to stay
// Running-but-not-Ready (AC-6) whenever the environment gate fails, and
// waiting on Ready there would just time out.
func isPodRunning(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning
}

// waitForPod polls namespace/name until check returns true or timeout
// elapses, failing t with the last observed pod on timeout.
func waitForPod(t *testing.T, ctx context.Context, clientset *kubernetes.Clientset, namespace, name string, timeout time.Duration, check func(*corev1.Pod) bool) *corev1.Pod {
	t.Helper()
	var last *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(pollCtx context.Context) (bool, error) {
		pod, err := clientset.CoreV1().Pods(namespace).Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil // not found yet (or transient) — keep polling
		}
		last = pod
		return check(pod), nil
	})
	if err != nil {
		t.Fatalf("wait for pod %s/%s: %v (last observed phase: %+v)", namespace, name, err, last)
	}
	return last
}

// waitForAgentPodRunning polls until this node's cpi-idle-agent DaemonSet
// pod exists and reaches phase Running. It deliberately does not wait for
// Ready: whenever the environment gate fails (Open Question 1's measured
// case on kind), Ready never happens by design (AC-6), so waiting for it
// here would just time out instead of letting the test move on to assert
// the fail-safe behavior that IS expected.
func waitForAgentPodRunning(t *testing.T, ctx context.Context, clientset *kubernetes.Clientset, timeout time.Duration) *corev1.Pod {
	t.Helper()
	var last *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(pollCtx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(agentNamespace).List(pollCtx, metav1.ListOptions{LabelSelector: agentLabelSelector})
		if err != nil || len(pods.Items) == 0 {
			return false, nil
		}
		last = &pods.Items[0]
		return isPodRunning(last), nil
	})
	if err != nil {
		t.Fatalf("wait for agent DaemonSet pod to reach Running: %v (last observed: %+v)", err, last)
	}
	return last
}

// applyConfigBase applies config/base (namespace, RBAC, DaemonSet) with
// `kubectl apply -k` — exactly the manifests a real deployment uses. These
// tests must prove production's own YAML works, not a modified copy.
func applyConfigBase(t *testing.T) {
	t.Helper()
	runCmd(t, "kubectl", kubectlArgs("apply", "-k", "../../config/base")...)
}

// deleteConfigBase best-effort tears down config/base for repeated local
// runs against a long-lived kind cluster. In CI the whole cluster is
// deleted right after the test job anyway, so this is a convenience for
// local iteration, not a correctness requirement.
func deleteConfigBase(t *testing.T) {
	t.Helper()
	cmd := exec.Command("kubectl", kubectlArgs("delete", "-k", "../../config/base", "--wait=false")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("cleanup: delete config/base: %v\n%s", err, out)
	}
}

// forcePullPolicyNever patches the deployed DaemonSet's imagePullPolicy to
// Never. config/base/daemonset.yaml intentionally leaves imagePullPolicy
// unset (defaulting to Always for the "latest" tag — correct for a real
// registry pull in production); a kind-loaded image is never on a
// registry, so without this test-only override every agent pod sits in
// ErrImagePull forever (measured while writing this suite).
func forcePullPolicyNever(t *testing.T) {
	t.Helper()
	runCmd(t, "kubectl", kubectlArgs(
		"-n", agentNamespace, "patch", "daemonset", agentDaemonSet,
		"--type=json",
		"-p", `[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]`,
	)...)
}

// proxyGet fetches path from podName's port through the API server's pod
// proxy subresource, returning combined output and kubectl's error.
//
// This goes through `kubectl get --raw .../proxy/...` rather than execing
// curl/wget inside the pod: the runtime image has no shell (see Dockerfile),
// so an in-pod exec is not available here at all, and the API server proxy
// reaches the same port a real client without a Service would.
func proxyGet(podName string, port int, path string) (string, error) {
	target := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/%s", agentNamespace, podName, port, path)
	cmd := exec.Command("kubectl", kubectlArgs("get", "--raw", target)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// agentLogs returns the current agent pod's logs.
func agentLogs(t *testing.T) string {
	t.Helper()
	return runCmd(t, "kubectl", kubectlArgs("-n", agentNamespace, "logs", "-l", agentLabelSelector, "--tail=100")...)
}
