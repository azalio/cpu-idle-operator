package apply

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
)

// TestIntegrationApplyBothTiersWritesRealFiles exercises Applier.Apply
// against a real filesystem fixture, through the production
// cgroup.WriteKnob path — including its INV-1 write-target guard — rather
// than a fake Writer. This proves the whole stack this subtask assembles
// (Snapshot, plan, guard, Recorder) converges to the right final file
// contents, complementing apply_test.go's fake-Writer tests, which prove
// the right *sequence* of calls but never touch a real file.
func TestIntegrationApplyBothTiersWritesRealFiles(t *testing.T) {
	root := t.TempDir()
	const uid = "77777777-7777-7777-7777-777777777777"
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "20", "100000 100000", "0")

	pod := testPod(uid, "500m", map[string]string{
		annotations.TierKey:  annotations.TierValueIdle,
		annotations.BurstKey: "",
	})
	recorder, events, _, _ := newTestObservers("node-a")
	applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)

	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	assertKnobContent(t, dir, KnobCPUMaxBurst, "100000")
	assertKnobContent(t, dir, KnobCPUIdle, "1")
	// cpu.weight is untouched by an install: the kernel only resets it on
	// cpu.idle's 1->0 transition (resolution T-005), never on 0->1, and
	// BuildPlan only ever plans a restore write alongside that specific
	// transition (plan.go) — see TestSeamWeightRestoredWhenOnlyTierRemoved
	// for the 1->0 case reached through this same Apply path.
	assertKnobContent(t, dir, KnobCPUWeight, "20")
}

// TestIntegrationApplyRevertBothTiersWritesRealFiles starts from a real
// fixture where both tiers are already active and applies a pod with
// neither annotation, proving the revert direction (INV-7's reversed
// order) converges to the right final file contents through the real
// writer too.
func TestIntegrationApplyRevertBothTiersWritesRealFiles(t *testing.T) {
	root := t.TempDir()
	const uid = "88888888-8888-8888-8888-888888888888"
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"1", "1", "100000 100000", "100000")

	pod := testPod(uid, "500m", nil)
	recorder, events, _, _ := newTestObservers("node-a")
	applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)

	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	assertKnobContent(t, dir, KnobCPUIdle, "0")
	assertKnobContent(t, dir, KnobCPUMaxBurst, "0")
}

// TestIntegrationApplyNoPlanTouchesNoFiles proves an already-converged pod
// causes zero writes even through the real Writer: every knob file's mtime
// content stays byte-for-byte what the fixture seeded.
func TestIntegrationApplyNoPlanTouchesNoFiles(t *testing.T) {
	root := t.TempDir()
	const uid = "99999999-9999-9999-9999-999999999999"
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"1", "1", "100000 100000", "100000")

	pod := testPod(uid, "500m", map[string]string{
		annotations.TierKey:  annotations.TierValueIdle,
		annotations.BurstKey: "",
	})
	recorder, events, _, _ := newTestObservers("node-a")
	applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)

	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	assertKnobContent(t, dir, KnobCPUIdle, "1")
	assertKnobContent(t, dir, KnobCPUWeight, "1")
	assertKnobContent(t, dir, KnobCPUMax, "100000 100000")
	assertKnobContent(t, dir, KnobCPUMaxBurst, "100000")
}

// TestSeamWeightRestoredWhenOnlyTierRemoved covers the seam between
// Reconciler's Apply/Revert routing and this package's write plan: a pod
// carrying both annotations (config/samples/pod-both.yaml's exact shape)
// that has only its tier annotation removed still requests burst, so it
// stays routed through Apply — never Revert, the only function that used
// to restore cpu.weight. Before BuildPlan itself decided when cpu.weight
// needs restoring (plan.go), Apply's plan never included it at all: the
// kernel resets cpu.weight to its own default on the very cpu.idle 1->0
// transition this scenario performs (resolution T-005), and nothing wrote
// it back, leaving the pod at a five-times-larger CPU share than its spec
// requests until some later pass happened to route it through Revert
// instead — which never happens on its own while burst is still requested.
func TestSeamWeightRestoredWhenOnlyTierRemoved(t *testing.T) {
	root := t.TempDir()
	const uid = "77777777-7777-7777-7777-777777777777"
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"1", "1", "100000 100000", "100000")

	pod := testPod(uid, "1", map[string]string{annotations.BurstKey: "true"})
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"),
	}

	recorder, events, _, _ := newTestObservers("node-a")
	applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertKnobContent(t, dir, KnobCPUIdle, "0")
	assertKnobContent(t, dir, KnobCPUWeight, "20")
}

func assertKnobContent(t *testing.T, dir, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s content = %q, want %q", name, got, want)
	}
}
