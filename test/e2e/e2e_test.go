//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/azalio/cpi-idle-operator/internal/annotations"
	"github.com/azalio/cpi-idle-operator/internal/apply"
	"github.com/azalio/cpi-idle-operator/internal/envgate"
	"github.com/azalio/cpi-idle-operator/internal/qos"
)

// TestKindApplyAndRevert deploys config/base's real DaemonSet into kind and
// exercises AC-10 against it (VC1). Setup builds no shortcuts around
// production's own manifests: `kubectl apply -k config/base` unmodified,
// only patched afterward to tolerate a kind-loaded (unregistered) image —
// see forcePullPolicyNever's doc comment.
//
// Immediately after the DaemonSet pod is up, this test re-runs the same
// path comparison TestPreflightKindCgroupViewConsistency performs and
// branches on the live result:
//
//   - converged: runs the full AC-10 positive scenario (apply idle tier,
//     read cpu.idle=1 from the node, remove the annotation, read cpu.idle=0
//     and the restored weight).
//   - diverged (the measured, documented state on kind today — Open
//     Question 1): runs assertFailSafeBehavior instead. This is not a
//     downgrade to "test nothing" — it proves the one thing that IS
//     achievable and still valuable on kind: a real DaemonSet, driven by a
//     real informer against a real API server, correctly self-diagnoses an
//     incompatible node and performs zero cgroup writes rather than
//     crash-looping or silently doing nothing unexplained (AC-6, INV-5).
//
// Either branch is a real assertion that must pass; this test never
// silently skips.
func TestKindApplyAndRevert(t *testing.T) {
	clientset := kubeClient(t)
	requireNodeReachable(t)
	ctx := t.Context()

	applyConfigBase(t)
	forcePullPolicyNever(t)
	t.Cleanup(func() { deleteConfigBase(t) })

	agentPod := waitForAgentPodRunning(t, ctx, clientset, podReadyTimeout)
	t.Logf("agent DaemonSet pod %s/%s is Running", agentPod.Namespace, agentPod.Name)

	ns := createTempNamespace(t, ctx, clientset, "cpi-e2e")
	defer deleteNamespace(t, clientset, ns)

	idlePod := applyProbePod(t, ctx, clientset, ns, "idle-probe", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	idlePod = waitForPod(t, ctx, clientset, ns, idlePod.Name, podReadyTimeout, isPodReady)

	agentPath := computeAgentPath(t, qos.ToCgroupClass(qos.ClassOf(idlePod.Spec)), string(idlePod.UID))

	if nodePathExists(agentPath) {
		t.Logf("cgroup view converged on this run: %s exists -- running the positive AC-10 scenario", agentPath)
		runPositiveTierScenario(t, ctx, clientset, agentPath, idlePod)
		return
	}

	t.Logf("cgroup view diverged (Open Question 1, see preflight_test.go): %s does not exist -- "+
		"running the fail-safe assertions instead of the positive scenario", agentPath)
	assertFailSafeBehavior(t, agentPod.Name, idlePod)
}

// runPositiveTierScenario exercises AC-10 for real: apply the idle tier
// through the live DaemonSet, read the actual cpu.idle byte back from the
// node's cgroupfs, remove the annotation, and confirm both cpu.idle=0 and
// the request-derived cpu.weight are restored.
//
// It only runs when TestKindApplyAndRevert's own preflight comparison
// converges -- which it does not on kind today (Open Question 1) -- so
// this code exists, compiles and is vetted on every CI run without
// currently executing. It activates automatically the moment kind's node
// layout ever matches production's, with no other change required.
func runPositiveTierScenario(t *testing.T, ctx context.Context, clientset *kubernetes.Clientset, agentPath string, pod *corev1.Pod) {
	t.Helper()

	waitForNodeFileValue(t, agentPath, apply.KnobCPUIdle, "1", podReadyTimeout)
	t.Logf("cpu.idle=1 confirmed at %s after applying %s=%s", agentPath, annotations.TierKey, annotations.TierValueIdle)

	removeTierAnnotation(t, ctx, clientset, pod.Namespace, pod.Name)

	waitForNodeFileValue(t, agentPath, apply.KnobCPUIdle, "0", podReadyTimeout)

	wantWeight := fmt.Sprintf("%d", qos.RestoreWeight(pod.Spec))
	waitForNodeFileValue(t, agentPath, apply.KnobCPUWeight, wantWeight, podReadyTimeout)
	t.Logf("cpu.idle=0 and cpu.weight=%s confirmed at %s after removing the tier annotation", wantWeight, agentPath)
}

// assertFailSafeBehavior exercises what IS truthfully verifiable on kind
// today, given Open Question 1's measured divergence: the real DaemonSet
// must self-diagnose the incompatible node and fail safe rather than
// crash-loop or silently do nothing unexplained.
//
//   - AC-6: liveness (/healthz) must stay 200 -- a failed environment gate
//     must never look unhealthy to kubelet's liveness probe.
//   - readiness (/readyz) must stay non-200: the gate genuinely failed, and
//     asserting otherwise would be worse than not testing this at all.
//   - the agent's own logs must name the exact measured reason
//     (kubepods_missing), so a regression that changes *why* envgate fails
//     here does not silently pass as "still fails, who cares why".
//   - INV-5: given a real, annotated, idle-tier pod delivered through a
//     real informer watching a real API server, the agent must still
//     perform zero cgroup writes -- proven by reading cpu.idle back from
//     the pod-cgroup kubelet actually created (not the agent's unreachable
//     computed path) and confirming it never left kubelet's own default.
func assertFailSafeBehavior(t *testing.T, agentPodName string, idlePod *corev1.Pod) {
	t.Helper()

	if _, err := proxyGet(agentPodName, agentHealthPort, "healthz"); err != nil {
		t.Fatalf("AC-6: agent /healthz must always return 200, even with a failed environment gate; proxy call failed: %v", err)
	}

	if body, err := proxyGet(agentPodName, agentHealthPort, "readyz"); err == nil {
		t.Fatalf("agent /readyz unexpectedly succeeded (body: %q); this would mean Open Question 1 has converged -- "+
			"see preflight_test.go, and if so this branch should not be the one running", body)
	}

	logs := agentLogs(t)
	if !strings.Contains(logs, string(envgate.ReasonKubepodsMissing)) {
		t.Fatalf("expected agent logs to report reason=%s (the measured Open Question 1 divergence); got:\n%s",
			envgate.ReasonKubepodsMissing, logs)
	}
	t.Logf("confirmed AC-6 fail-safe behavior: /healthz=200, /readyz failed, logs report reason=%s", envgate.ReasonKubepodsMissing)

	realPath := findNodePath(escapeUID(string(idlePod.UID)))
	if realPath == "" {
		t.Fatalf("could not locate the real pod-cgroup kubelet created for %s/%s on the node -- cannot verify INV-5",
			idlePod.Namespace, idlePod.Name)
	}
	idleValue, err := readNodeFile(realPath + "/" + apply.KnobCPUIdle)
	if err != nil {
		t.Fatalf("read %s/%s on the node: %v", realPath, apply.KnobCPUIdle, err)
	}
	if idleValue != "0" {
		t.Fatalf("INV-5 violated: node's real pod-cgroup %s has cpu.idle=%q, but the environment gate failed and must perform zero writes",
			realPath, idleValue)
	}
	t.Logf("confirmed INV-5: real pod-cgroup %s still has cpu.idle=0 despite a live idle-tier annotated pod -- the agent wrote nothing", realPath)
}

// removeTierAnnotation clears annotations.TierKey from namespace/name via a
// JSON merge patch (a null value deletes the key), the same mechanism a
// real client removing the annotation would use.
func removeTierAnnotation(t *testing.T, ctx context.Context, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, annotations.TierKey)
	_, err := clientset.CoreV1().Pods(namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("remove tier annotation from %s/%s: %v", namespace, name, err)
	}
}

// waitForNodeFileValue polls path/knob inside the kind node container until
// it reads exactly want or timeout elapses.
func waitForNodeFileValue(t *testing.T, path, knob, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = readNodeFile(path + "/" + knob)
		if lastErr == nil && last == want {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for %s/%s == %q; last read: %q (err: %v)", path, knob, want, last, lastErr)
}
