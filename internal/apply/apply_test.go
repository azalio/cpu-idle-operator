package apply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/azalio/cpi-idle-operator/internal/annotations"
	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/observe"
)

// fakeWriteCall is one recorded Writer.WriteKnob invocation, in the exact
// order it happened.
type fakeWriteCall struct {
	root, dir, name, value string
}

// fakeWriter is a journaling Writer: it records every WriteKnob call it
// receives instead of touching a filesystem, and never fails unless a test
// pre-loads an error for a specific knob name via results. AC-13/INV-7
// requires proving the *sequence* of Applier's writes, which no assertion
// against a real filesystem's final contents can distinguish from a
// reordered-but-converging implementation.
type fakeWriter struct {
	calls   []fakeWriteCall
	results map[string]error
}

func (f *fakeWriter) WriteKnob(root, kubepodsName, dir, name, value string) error {
	f.calls = append(f.calls, fakeWriteCall{root, dir, name, value})
	if f.results != nil {
		if err, ok := f.results[name]; ok {
			return err
		}
	}
	return nil
}

// fakeEventCall is one recorded record.EventRecorder call.
type fakeEventCall struct {
	object    runtime.Object
	eventType string
	reason    string
	message   string
}

// fakeEventRecorder implements client-go's record.EventRecorder, capturing
// every call instead of serializing to a string, so a test can assert the
// exact reason and involved object each Event carried.
type fakeEventRecorder struct {
	calls []fakeEventCall
}

func (f *fakeEventRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	f.calls = append(f.calls, fakeEventCall{object, eventtype, reason, message})
}

func (f *fakeEventRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any) {
	f.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (f *fakeEventRecorder) AnnotatedEventf(object runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...any) {
	f.Eventf(object, eventtype, reason, messageFmt, args...)
}

// newTestObservers builds a Recorder and an EventRecorder that share one
// fakeEventRecorder, the same wiring NewApplier expects from its caller —
// see Applier.events' doc comment for why the two are separate handles.
func newTestObservers(node string) (*observe.Recorder, *observe.EventRecorder, *fakeEventRecorder, *prometheus.Registry) {
	registry := prometheus.NewRegistry()
	fake := &fakeEventRecorder{}
	recorder := observe.NewRecorder(registry, fake, node)
	events := observe.NewEventRecorder(fake)
	return recorder, events, fake, registry
}

// newTestApplier builds an Applier with writer injected directly (same
// package, so the unexported field is reachable), bypassing NewApplier's
// production cgroupWriter.
func newTestApplier(cgroupRoot string, driver cgroup.Driver, writer Writer, recorder *observe.Recorder, events *observe.EventRecorder) *Applier {
	return &Applier{
		cgroupRoot:   cgroupRoot,
		kubepodsName: cgroup.DefaultKubepodsName,
		driver:       driver,
		writer:       writer,
		recorder:     recorder,
		events:       events,
	}
}

// testPod builds a minimal single-container Burstable-shaped pod (a
// positive CPU limit, no CPU request) carrying annos.
func testPod(uid, cpuLimit string, annos map[string]string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-1",
			Namespace:   "prod",
			UID:         types.UID(uid),
			Annotations: annos,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	if cpuLimit != "" {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpuLimit),
		}
	}
	return pod
}

// seedPodCgroup creates dir (computed the same way Applier computes it) and
// writes the four knob files Snapshot reads. It returns dir so the test can
// assert Writer calls against it.
func seedPodCgroup(t *testing.T, root string, driver cgroup.Driver, qosClass cgroup.QoSClass, uid string, idle, weight, max, burst string) string {
	t.Helper()
	dir, err := cgroup.PodCgroupPath(root, cgroup.DefaultKubepodsName, driver, qosClass, uid)
	if err != nil {
		t.Fatalf("PodCgroupPath: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	files := map[string]string{
		KnobCPUIdle:     idle,
		KnobCPUWeight:   weight,
		KnobCPUMax:      max,
		KnobCPUMaxBurst: burst,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

// TestVC1BurstEqualsQuota covers VC1 [AC-3]: a pod requesting burst with a
// CPU limit gets cpu.max.burst set to exactly the quota cpu.max reports —
// the value is never invented or derived from the annotation (SC-2) — and
// when cpu.max reports "max" (no quota configured), no burst write happens
// at all.
func TestVC1BurstEqualsQuota(t *testing.T) {
	t.Run("test_vc1_burst_equals_quota", func(t *testing.T) {
		root := t.TempDir()
		const uid = "11111111-1111-1111-1111-111111111111"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "20", "100000 100000", "0")

		pod := testPod(uid, "500m", map[string]string{annotations.BurstKey: ""})
		writer := &fakeWriter{}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

		if err := applier.Apply(context.Background(), pod); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}

		if len(writer.calls) != 1 {
			t.Fatalf("writer.calls = %+v, want exactly 1 (cpu.max.burst)", writer.calls)
		}
		call := writer.calls[0]
		if call.name != KnobCPUMaxBurst || call.value != "100000" {
			t.Errorf("write = %+v, want {name: %q, value: \"100000\"}", call, KnobCPUMaxBurst)
		}
	})

	t.Run("cpu_max_unbounded_means_no_burst_write", func(t *testing.T) {
		root := t.TempDir()
		const uid = "22222222-2222-2222-2222-222222222222"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "20", "max 100000", "0")

		pod := testPod(uid, "500m", map[string]string{annotations.BurstKey: ""})
		writer := &fakeWriter{}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

		if err := applier.Apply(context.Background(), pod); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}

		if len(writer.calls) != 0 {
			t.Fatalf("writer.calls = %+v, want none: cpu.max reports no quota", writer.calls)
		}
	})
}

// TestVC4UnknownValueVsMissingCgroup covers VC4 [AC-16]: two independent
// scenarios, each its own pod and directory, proving the two silences AC-16
// requires stay distinguishable rather than collapsing into one behavior.
func TestVC4UnknownValueVsMissingCgroup(t *testing.T) {
	t.Run("test_vc4_unknown_tier_value_gives_zero_writes_and_one_event", func(t *testing.T) {
		root := t.TempDir()
		const uid = "33333333-3333-3333-3333-333333333333"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBestEffort, uid,
			"0", "1", "max 100000", "0")

		pod := testPod(uid, "", map[string]string{annotations.TierKey: "aggressive"})
		writer := &fakeWriter{}
		recorder, events, fakeEvents, _ := newTestObservers("node-a")
		applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

		if err := applier.Apply(context.Background(), pod); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}

		if len(writer.calls) != 0 {
			t.Fatalf("writer.calls = %+v, want none for an unrecognized tier value", writer.calls)
		}
		if len(fakeEvents.calls) != 1 {
			t.Fatalf("event calls = %+v, want exactly 1", fakeEvents.calls)
		}
		if got := fakeEvents.calls[0].reason; got != string(observe.ReasonTierValueUnknown) {
			t.Errorf("event reason = %q, want %q", got, observe.ReasonTierValueUnknown)
		}
		if fakeEvents.calls[0].object != pod {
			t.Errorf("event involvedObject = %#v, want pod", fakeEvents.calls[0].object)
		}
	})

	t.Run("test_vc4_missing_pod_cgroup_gives_zero_writes_zero_events_nil_error", func(t *testing.T) {
		root := t.TempDir()
		const uid = "44444444-4444-4444-4444-444444444444"
		// Deliberately not seeded: the pod cgroup directory never existed
		// (or has already been removed), simulating the pod having been
		// deleted between the informer handing it to the caller and Apply
		// running.
		pod := testPod(uid, "", map[string]string{annotations.TierKey: annotations.TierValueIdle})
		writer := &fakeWriter{}
		recorder, events, fakeEvents, _ := newTestObservers("node-a")
		applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

		err := applier.Apply(context.Background(), pod)
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
		if len(writer.calls) != 0 {
			t.Fatalf("writer.calls = %+v, want none", writer.calls)
		}
		if len(fakeEvents.calls) != 0 {
			t.Fatalf("event calls = %+v, want none", fakeEvents.calls)
		}
	})
}

// TestVC5EveryWriteLeavesTrace covers VC5 [CCR-1]: for a pod requesting
// both tiers from a fully unapplied state, the number of counter
// increments and the number of Events must equal the number of writes
// Applier actually executed — no write happens without a paired trace, and
// no trace is emitted without a write behind it. It also re-confirms the
// INV-7 sequence at the Applier level (not just BuildPlan's return value),
// against the fake Writer's own call journal.
func TestVC5EveryWriteLeavesTrace(t *testing.T) {
	root := t.TempDir()
	const uid = "55555555-5555-5555-5555-555555555555"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "20", "100000 100000", "0")

	pod := testPod(uid, "500m", map[string]string{
		annotations.TierKey:  annotations.TierValueIdle,
		annotations.BurstKey: "",
	})
	writer := &fakeWriter{}
	recorder, events, fakeEvents, registry := newTestObservers("node-a")
	applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	writesExecuted := len(writer.calls)
	if writesExecuted != 2 {
		t.Fatalf("writer.calls = %+v, want exactly 2 (cpu.max.burst, cpu.idle)", writer.calls)
	}
	if writer.calls[0].name != KnobCPUMaxBurst || writer.calls[1].name != KnobCPUIdle {
		t.Fatalf("write order = [%s, %s], want [%s, %s]",
			writer.calls[0].name, writer.calls[1].name, KnobCPUMaxBurst, KnobCPUIdle)
	}

	if got := len(fakeEvents.calls); got != writesExecuted {
		t.Errorf("event calls = %d, want %d (one per executed write)", got, writesExecuted)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	var counterIncrements float64
	for _, family := range families {
		if family.GetName() != "cpi_tier_apply_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			counterIncrements += metric.GetCounter().GetValue()
		}
	}
	if counterIncrements != float64(writesExecuted) {
		t.Errorf("cpi_tier_apply_total total increments = %v, want %d", counterIncrements, writesExecuted)
	}
}

// TestApplyEINVALIsRejectedNotRetried covers the described kernel-rejection
// path: the kernel refusing a write (e.g. lowering cpu.max below an
// already-set cpu.max.burst) is recorded once as a rejection, not treated
// as a transient error to retry against the same stale plan.
func TestApplyEINVALIsRejectedNotRetried(t *testing.T) {
	root := t.TempDir()
	const uid = "66666666-6666-6666-6666-666666666666"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "20", "100000 100000", "0")

	pod := testPod(uid, "500m", map[string]string{
		annotations.TierKey:  annotations.TierValueIdle,
		annotations.BurstKey: "",
	})
	writer := &fakeWriter{results: map[string]error{
		KnobCPUMaxBurst: fmt.Errorf("cgroup: write knob cpu.max.burst: %w", syscall.EINVAL),
	}}
	recorder, events, fakeEvents, registry := newTestObservers("node-a")
	applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v, want nil (EINVAL is recorded, not surfaced for a retry loop)", err)
	}

	// Only the failing cpu.max.burst write was attempted; cpu.idle — later
	// in plan order — must not be attempted once the plan has already hit
	// a kernel rejection.
	if len(writer.calls) != 1 {
		t.Fatalf("writer.calls = %+v, want exactly 1 (the rejected cpu.max.burst write)", writer.calls)
	}

	if len(fakeEvents.calls) != 1 {
		t.Fatalf("event calls = %+v, want exactly 1", fakeEvents.calls)
	}
	if got := fakeEvents.calls[0].reason; got != string(observe.ReasonWriteRejected) {
		t.Errorf("event reason = %q, want %q", got, observe.ReasonWriteRejected)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	var found bool
	for _, family := range families {
		if family.GetName() != "cpi_tier_apply_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, lp := range metric.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["result"] == string(observe.TierApplyResultRejected) && labels["reason"] == string(observe.TierApplyReasonEINVAL) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("cpi_tier_apply_total has no series with result=rejected reason=einval; families = %+v", families)
	}
}

// TestApplyWeightRestoreReportsReverted covers the seam TestSeamWeightRestoredWhenOnlyTierRemoved
// (apply_integration_test.go) reaches through the real Writer but does not
// itself inspect: when Apply's own plan restores cpu.weight (a pod that
// keeps requesting burst while only its tier annotation is removed), that
// write must be reported as TierReverted, matching every cpu.weight write
// Revert itself ever reports — never TierApplied, which writeIsInstall's
// plain "value != 0" rule would wrongly assign since a restored weight is
// never the string "0".
func TestApplyWeightRestoreReportsReverted(t *testing.T) {
	root := t.TempDir()
	const uid = "88888888-1111-1111-1111-111111111111"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"1", "1", "100000 100000", "100000")

	pod := testPod(uid, "1", map[string]string{annotations.BurstKey: ""})
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"),
	}
	writer := &fakeWriter{}
	recorder, events, fakeEvents, _ := newTestObservers("node-a")
	applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

	if err := applier.Apply(context.Background(), pod); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if len(writer.calls) != 2 {
		t.Fatalf("writer.calls = %+v, want exactly 2 (cpu.idle, cpu.weight); cpu.max.burst already matches quota", writer.calls)
	}
	if writer.calls[0].name != KnobCPUIdle || writer.calls[1].name != KnobCPUWeight {
		t.Fatalf("write order = [%s, %s], want [%s, %s]",
			writer.calls[0].name, writer.calls[1].name, KnobCPUIdle, KnobCPUWeight)
	}

	if len(fakeEvents.calls) != 2 {
		t.Fatalf("event calls = %+v, want exactly 2", fakeEvents.calls)
	}
	for i, call := range fakeEvents.calls {
		if call.reason != string(observe.ReasonTierReverted) {
			t.Errorf("event[%d].reason = %q, want %q (a weight restore is a revert-side effect of clearing cpu.idle, never an install)", i, call.reason, observe.ReasonTierReverted)
		}
	}
}
