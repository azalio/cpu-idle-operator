//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/apply"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// TestKindApplyAndRevert deploys config/base's real DaemonSet into kind and
// exercises AC-10 against it (VC1) for real. Setup builds no shortcuts
// around production's own manifests: `kubectl apply -k config/base`
// unmodified, only patched afterward for two kind-only reasons —
// forcePullPolicyNever (a kind-loaded image is never on a registry) and
// patchKindCgroupFlags (kind's kubelet lays out kubepods cgroups
// differently than a real production kubelet, see helpers.go's
// kindCgroupRoot/kindKubepodsName doc comment and
// TestPreflightKindCgroupViewConsistency). config/base/daemonset.yaml
// itself never carries either override: production clusters keep the
// plain defaults (README's Requirements section).
//
// TestPreflightKindCgroupViewConsistency is this test's own precondition:
// it already proves, with a throwaway probe pod, that the agent's computed
// path converges with kind's real kubelet layout once patched this way. If
// that precondition ever regresses, it fails as its own, more specific,
// merge-blocking check — this test does not re-litigate convergence, it
// exercises the full DaemonSet apply/revert cycle on top of it: apply the
// idle tier, read cpu.idle=1 from the node, remove the annotation, and read
// cpu.idle=0 with the restored weight back from the node.
func TestKindApplyAndRevert(t *testing.T) {
	clientset := kubeClient(t)
	requireNodeReachable(t)
	ctx := t.Context()

	applyConfigBase(t)
	forcePullPolicyNever(t)
	patchKindCgroupFlags(t)
	t.Cleanup(func() { deleteConfigBase(t) })

	agentPod := waitForAgentPodRunning(t, ctx, clientset, podReadyTimeout)
	t.Logf("agent DaemonSet pod %s/%s is Running", agentPod.Namespace, agentPod.Name)

	ns := createTempNamespace(t, ctx, clientset, "cpu-e2e")
	defer deleteNamespace(t, clientset, ns)

	idlePod := applyProbePod(t, ctx, clientset, ns, "idle-probe", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	idlePod = waitForPod(t, ctx, clientset, ns, idlePod.Name, podReadyTimeout, isPodReady)

	agentPath := computeAgentPath(t, kindCgroupRoot, kindKubepodsName, qos.ToCgroupClass(qos.ClassOf(idlePod.Spec)), string(idlePod.UID))

	if !nodePathExists(agentPath) {
		realPath := findNodePath(escapeUID(string(idlePod.UID)))
		t.Fatalf("agent-computed pod cgroup path %s does not exist on the node (actual path found: %q); "+
			"TestPreflightKindCgroupViewConsistency should have caught this divergence first — see its doc comment",
			agentPath, realPath)
	}

	runPositiveTierScenario(t, ctx, clientset, agentPath, idlePod)
}

// runPositiveTierScenario exercises AC-10 for real: apply the idle tier
// through the live DaemonSet, read the actual cpu.idle byte back from the
// node's cgroupfs, remove the annotation, and confirm both cpu.idle=0 and
// the request-derived cpu.weight are restored.
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
