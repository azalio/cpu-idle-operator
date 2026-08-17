//go:build e2e

package e2e

import (
	"testing"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

// TestPreflightKindCgroupViewConsistency is the mandatory first e2e check
// (VC2): does the pod-cgroup path this agent computes from
// config/base's own default --cgroup-root match the path kubelet actually
// creates, as seen directly inside the kind node container? Every other
// assertion in this package depends on the answer, so this test is meant
// to be read start to finish when it fails.
//
// # Measured answer (kind v0.29.0, kindest/node:v1.33.1, checked 2026-08-17)
//
// No, it does not converge. kind's own kubeadm config sets the
// control-plane kubelet's KubeletConfiguration.cgroupRoot to "/kubelet"
// (confirmed by reading /var/lib/kubelet/config.yaml inside the node
// container), which nests the entire kubepods hierarchy one level deeper
// and prefixes every intermediate slice component with "kubelet-":
//
//	agent-computed (prodCgroupRoot, production's own default):
//	  /sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice
//	actually created by kind's kubelet on the node:
//	  /sys/fs/cgroup/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-<qos>.slice/kubelet-kubepods-<qos>-pod<uid>.slice
//
// This is not a bind-mount visibility problem: the divergence is visible
// directly inside the kind node container itself, with no pod or hostPath
// mount involved at all. The extraMounts recommendation carried over from
// earlier research (see
// .map/wayfind/cpu-idle-operator/resolutions/T-012.md) therefore does not
// apply to the actual failure mode found here and was deliberately left
// out of kind-config.yaml — bind-mounting /sys/fs/cgroup differently would
// not change kubelet's slice naming at all.
//
// Deployed for real as config/base's own DaemonSet in this same
// environment, the agent's own internal/envgate.Check independently
// reaches the identical conclusion at runtime: Ready=false,
// Reason=kubepods_missing (see TestKindApplyAndRevert, which asserts this
// from the live pod's logs).
//
// # Open Question 1
//
// This divergence is the spec's Open Question 1
// (.map/default/spec_default.md) and the reason ST-013 was marked
// provisional. Per the decision recorded there and in T-012.md, a failure
// here does NOT fail the subtask: the blocking kind e2e gate is replaced
// by the fail-safe assertions TestKindApplyAndRevert runs instead of the
// positive tier-apply/revert scenario, and AC-10's positive scenario stays
// covered by hack/stand-probe.sh on a real node (already measured there,
// see resolutions/T-005.md). CI treats exactly this documented failure as
// non-blocking — see .github/workflows/ci.yaml's e2e job — while still
// failing the build on any other preflight outcome, including this test
// unexpectedly passing (which would mean Open Question 1 has a new answer
// and the positive scenario should be turned back on as a merge blocker).
func TestPreflightKindCgroupViewConsistency(t *testing.T) {
	clientset := kubeClient(t)
	requireNodeReachable(t)
	ctx := t.Context()

	ns := createTempNamespace(t, ctx, clientset, "cpi-preflight")
	defer deleteNamespace(t, clientset, ns)

	pod := applyProbePod(t, ctx, clientset, ns, "preflight-probe", "500m", nil)
	pod = waitForPod(t, ctx, clientset, ns, pod.Name, podReadyTimeout, isPodReady)

	agentPath := computeAgentPath(t, cgroup.QoSBurstable, string(pod.UID))
	if nodePathExists(agentPath) {
		t.Logf("cgroup view converged: %s exists on the node -- Open Question 1 now answers YES on this kind image; "+
			"TestKindApplyAndRevert's positive apply/revert branch should now be running instead of the fail-safe branch.",
			agentPath)
		return
	}

	realPath := findNodePath(escapeUID(string(pod.UID)))
	t.Fatalf(`OPEN QUESTION 1: kind's cgroup view does NOT match the agent's computed path.

agent-computed path (does not exist on the node):
  %s

actual path kubelet created on the node:
  %s

This is the spec's Open Question 1 (.map/default/spec_default.md): kind's
own kubeadm config nests kubepods under a "kubelet.slice/kubelet-kubepods..."
prefix instead of the plain "kubepods.slice/..." layout a production
kubelet (default cgroupRoot) produces. See this test's doc comment for the
full measurement and .map/wayfind/cpu-idle-operator/resolutions/T-012.md
for the decision this failure triggers: the kind e2e gate is replaced by
TestKindApplyAndRevert's fail-safe assertions plus hack/stand-probe.sh on a
real node -- not by patching kind's kubeadm config to work around it.`,
		agentPath, realPath)
}
