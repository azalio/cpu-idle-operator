//go:build e2e

package e2e

import (
	"testing"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

// TestPreflightKindCgroupViewConsistency is the mandatory first e2e check
// (VC2): does the pod-cgroup path this agent computes for kind's own
// configuration (kindCgroupRoot/kindKubepodsName, see helpers.go) match the
// path kubelet actually creates, as seen directly inside the kind node
// container? TestKindApplyAndRevert's positive AC-10 scenario depends on
// this converging, so this test is meant to be read start to finish when
// it fails.
//
// # Formerly Open Question 1 (now closed)
//
// kind's own kubeadm config sets the control-plane kubelet's
// KubeletConfiguration.cgroupRoot to "/kubelet" (confirmed by reading
// /var/lib/kubelet/config.yaml inside the node container), which nests the
// entire kubepods hierarchy one level deeper and prefixes every
// intermediate slice component with "kubelet-":
//
//	config/base's own production defaults (never converge on kind):
//	  /sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice
//	actually created by kind's kubelet on the node:
//	  /sys/fs/cgroup/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-<qos>.slice/kubelet-kubepods-<qos>-pod<uid>.slice
//
// This is not a bind-mount visibility problem: the divergence is visible
// directly inside the kind node container itself, with no pod or hostPath
// mount involved at all — see kind-config.yaml's own doc comment for why no
// extraMounts workaround was ever applied here.
//
// This used to be the spec's Open Question 1
// (.map/default/spec_default.md, .map/wayfind/cpu-idle-operator/resolutions/T-012.md):
// production's own defaults cannot be made to match kind's kubelet layout,
// full stop. It is closed not by changing config/base (which must keep
// shipping the defaults that are correct for a real production kubelet —
// see README's Supported Environments section) but by making the top-level
// kubepods cgroup name configurable (--kubepods-name): pointed at
// kindCgroupRoot with kindKubepodsName, the agent's own
// cgroup.PodCgroupPath computes exactly the path measured above, with no
// double-counted "kubelet.slice" component (internal/cgroup/path_test.go's
// own TestVC5KindMeasuredPathNoDoubleRoot proves this in isolation, without
// a live cluster). This test proves it converges against the real kind
// node, not just in a unit test fixture. Measured on kind v0.29.0,
// kindest/node:v1.33.1.
func TestPreflightKindCgroupViewConsistency(t *testing.T) {
	clientset := kubeClient(t)
	requireNodeReachable(t)
	ctx := t.Context()

	ns := createTempNamespace(t, ctx, clientset, "cpi-preflight")
	defer deleteNamespace(t, clientset, ns)

	pod := applyProbePod(t, ctx, clientset, ns, "preflight-probe", "500m", nil)
	pod = waitForPod(t, ctx, clientset, ns, pod.Name, podReadyTimeout, isPodReady)

	agentPath := computeAgentPath(t, kindCgroupRoot, kindKubepodsName, cgroup.QoSBurstable, string(pod.UID))
	if nodePathExists(agentPath) {
		t.Logf("cgroup view converged: %s exists on the node, using kindCgroupRoot/kindKubepodsName", agentPath)
		return
	}

	realPath := findNodePath(escapeUID(string(pod.UID)))
	t.Fatalf(`kind's cgroup view does NOT match the agent's computed path, even with kindCgroupRoot/kindKubepodsName.

agent-computed path (does not exist on the node):
  %s

actual path kubelet created on the node:
  %s

This is a regression: either this kind image changed its kubelet cgroup
layout again (update kindCgroupRoot/kindKubepodsName in helpers.go and this
doc comment to match), or internal/cgroup.PodCgroupPath's systemd-driver
double-root fix (path.go's systemdPodCgroupPath) broke. See
internal/cgroup/path_test.go's TestVC5KindMeasuredPathNoDoubleRoot for the
isolated unit-test proof this is meant to match.`,
		agentPath, realPath)
}
