package observe

import (
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeEventRecorder captures every call instead of only serializing to a
// string the way client-go's record.FakeRecorder does. VC3 needs to assert
// the exact object each Event was raised against (involvedObject = the pod
// passed to Applied), which a formatted string throws away.
type fakeEventRecorder struct {
	calls []fakeEventCall
}

type fakeEventCall struct {
	object    runtime.Object
	eventType string
	reason    string
	message   string
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

// TestVC3MetricAndEventAreAtomic checks that Recorder.Applied's one call
// produces exactly the pair CCR-1 requires: one cpi_tier_apply_total
// increment and exactly one Event, raised against the same pod passed in —
// never a metric increment with no Event, or an Event with no metric.
func TestVC3MetricAndEventAreAtomic(t *testing.T) {
	registry := prometheus.NewRegistry()
	fake := &fakeEventRecorder{}
	recorder := NewRecorder(registry, fake, "node-a")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod", UID: "pod-uid-1"},
	}

	recorder.Applied(pod, "cpu.idle", "applied", "ok")

	if got := len(fake.calls); got != 1 {
		t.Fatalf("event calls = %d, want exactly 1", got)
	}
	call := fake.calls[0]
	gotPod, ok := call.object.(*corev1.Pod)
	if !ok || gotPod != pod {
		t.Errorf("event involvedObject = %#v, want the same *corev1.Pod passed to Applied", call.object)
	}
	if call.reason != string(ReasonTierApplied) {
		t.Errorf("event reason = %q, want %q", call.reason, ReasonTierApplied)
	}
	if call.eventType != corev1.EventTypeNormal {
		t.Errorf("event type = %q, want %q", call.eventType, corev1.EventTypeNormal)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	var applyFamily *dto.MetricFamily
	for _, family := range families {
		if family.GetName() == "cpi_tier_apply_total" {
			applyFamily = family
		}
	}
	if applyFamily == nil {
		t.Fatalf("cpi_tier_apply_total family missing from Gather() output")
	}
	if got := len(applyFamily.GetMetric()); got != 1 {
		t.Fatalf("cpi_tier_apply_total series count = %d, want exactly 1", got)
	}

	metric := applyFamily.GetMetric()[0]
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Errorf("cpi_tier_apply_total value = %v, want 1", got)
	}
	wantLabels := map[string]string{
		"node": "node-a", "namespace": "prod", "qos_class": "BestEffort", "result": "applied", "reason": "ok",
	}
	gotLabels := map[string]string{}
	for _, labelPair := range metric.GetLabel() {
		gotLabels[labelPair.GetName()] = labelPair.GetValue()
	}
	if !reflect.DeepEqual(gotLabels, wantLabels) {
		t.Errorf("cpi_tier_apply_total labels = %v, want %v", gotLabels, wantLabels)
	}
}

// TestRecorderNormalizesUnrecognizedReasonToOther checks that Recorder.record
// collapses any reason string outside this package's bounded vocabulary
// (HC-5): two different, unrecognized reason values — as the kernel would
// produce for distinct cgroup-write failures — must land on the same
// cpi_tier_apply_total series with reason="other" instead of minting a new
// series per unique error text.
func TestRecorderNormalizesUnrecognizedReasonToOther(t *testing.T) {
	registry := prometheus.NewRegistry()
	fake := &fakeEventRecorder{}
	recorder := NewRecorder(registry, fake, "node-a")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod", UID: "pod-uid-1"},
	}

	recorder.Rejected(pod, "cpu.idle", "rejected", "write /sys/fs/cgroup/.../cpu.idle: invalid argument")
	recorder.Rejected(pod, "cpu.idle", "rejected", "write /sys/fs/cgroup/.../cpu.idle: no such file or directory")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	var applyFamily *dto.MetricFamily
	for _, family := range families {
		if family.GetName() == "cpi_tier_apply_total" {
			applyFamily = family
		}
	}
	if applyFamily == nil {
		t.Fatalf("cpi_tier_apply_total family missing from Gather() output")
	}
	if got := len(applyFamily.GetMetric()); got != 1 {
		t.Fatalf("cpi_tier_apply_total series count = %d, want exactly 1 (two distinct kernel error texts must collapse into one \"other\" series)", got)
	}

	metric := applyFamily.GetMetric()[0]
	if got := metric.GetCounter().GetValue(); got != 2 {
		t.Errorf("cpi_tier_apply_total value = %v, want 2", got)
	}
	gotLabels := map[string]string{}
	for _, labelPair := range metric.GetLabel() {
		gotLabels[labelPair.GetName()] = labelPair.GetValue()
	}
	if got := gotLabels["reason"]; got != string(TierApplyReasonOther) {
		t.Errorf("reason label = %q, want %q", got, TierApplyReasonOther)
	}
	if got := gotLabels["result"]; got != string(TierApplyResultRejected) {
		t.Errorf("result label = %q, want %q", got, TierApplyResultRejected)
	}
}

// TestNormalizeTierApplyReasonKnownValuesRoundTrip checks that every member
// of TierApplyReason's bounded vocabulary passes through
// normalizeTierApplyReason unchanged. If a future edit adds a reason
// constant without also listing it in normalizeTierApplyReason's switch, it
// would silently collapse to TierApplyReasonOther and lose the diagnostic
// value the vocabulary exists to provide — this test catches that
// mismatch.
func TestNormalizeTierApplyReasonKnownValuesRoundTrip(t *testing.T) {
	knownReasons := []TierApplyReason{
		TierApplyReasonOK,
		TierApplyReasonValueUnknown,
		TierApplyReasonLimitsCPUMissing,
		TierApplyReasonEnvironmentUnsupported,
		TierApplyReasonCgroupGone,
		TierApplyReasonNotPodCgroup,
		TierApplyReasonEINVAL,
	}
	for _, reason := range knownReasons {
		t.Run(string(reason), func(t *testing.T) {
			if got := normalizeTierApplyReason(string(reason)); got != reason {
				t.Errorf("normalizeTierApplyReason(%q) = %q, want unchanged %q", reason, got, reason)
			}
		})
	}
}

// TestNewRecorderFromMetricsSharesOneRegistry covers the fix for the
// double-Prometheus-registry defect: before NewRecorderFromMetrics existed,
// a caller that already had a Metrics bundle registered on a registry (the
// way Reconciler does) had no way to hand that same bundle to a Recorder,
// so the only option was a second, dedicated registry merged at scrape
// time via prometheus.Gatherers -- two registries that would fail the
// *entire* /metrics scrape, not just one family, the moment they ever
// defined an overlapping series (Gather() errors atomically). This test
// proves the fix: a Metrics bundle registered once, then handed to both a
// direct caller (as Reconciler reads metrics.PodsInTier) and a
// NewRecorderFromMetrics-built Recorder (as the Applier path does),
// coexist on the one registry with no registration panic and no duplicate
// family in Gather()'s output.
func TestNewRecorderFromMetricsSharesOneRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	fake := &fakeEventRecorder{}
	recorder := NewRecorderFromMetrics(metrics, fake, "node-a")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod", UID: "pod-uid-1"},
	}

	// Touch a metric the way Reconciler does: directly through the shared
	// Metrics bundle, never through Recorder.
	metrics.PodsInTier.WithLabelValues("node-a", "prod", "Burstable", "idle").Set(1)
	// Touch a metric the way the Applier path does: through Recorder,
	// which must be writing into the very same Metrics instance.
	recorder.Applied(pod, "cpu.idle", "applied", "ok")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v, want no error: NewRecorderFromMetrics must not register a second bundle on registry", err)
	}

	seen := map[string]int{}
	for _, family := range families {
		seen[family.GetName()]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("metric family %q appeared %d times in Gather() output, want exactly 1 (duplicate registration)", name, count)
		}
	}
	if seen["cpi_pods_in_tier"] != 1 {
		t.Errorf("Gather() output = %v, want cpi_pods_in_tier (Reconciler-side write) present exactly once", seen)
	}
	if seen["cpi_tier_apply_total"] != 1 {
		t.Errorf("Gather() output = %v, want cpi_tier_apply_total (Recorder-side write) present exactly once", seen)
	}
}

// TestNormalizeTierApplyReasonUnknownTextCollapsesToOther checks that
// arbitrary kernel-error text outside the bounded vocabulary — not just the
// two fixtures TestRecorderNormalizesUnrecognizedReasonToOther exercises
// end-to-end — still normalizes to TierApplyReasonOther rather than passing
// through as its own label value.
func TestNormalizeTierApplyReasonUnknownTextCollapsesToOther(t *testing.T) {
	unknownReasons := []string{
		"",
		"write /sys/fs/cgroup/.../cpu.idle: permission denied",
		"context deadline exceeded",
		"ENOENT",
	}
	for _, reason := range unknownReasons {
		t.Run(reason, func(t *testing.T) {
			if got := normalizeTierApplyReason(reason); got != TierApplyReasonOther {
				t.Errorf("normalizeTierApplyReason(%q) = %q, want %q", reason, got, TierApplyReasonOther)
			}
		})
	}
}
