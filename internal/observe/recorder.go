package observe

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// Recorder pairs a cpu_tier_apply_total increment with exactly one Event on
// the pod involved, for each of its outcome methods — the single
// enforcement point for CCR-1 ("every real cgroup change carries an Event
// and a metric increment, never one without the other"). Every other
// package in this operator that needs to report a tier-apply outcome must
// go through Recorder rather than touching Metrics or EventRecorder
// separately, so it is structurally impossible to increment the counter
// and forget the Event, or the reverse.
type Recorder struct {
	metrics *Metrics
	events  *EventRecorder
	node    string
}

// NewRecorder builds a Recorder that reports as node, registering a fresh
// Metrics bundle on registry and recording Events through eventRecorder.
// Callers that already have a Metrics bundle registered elsewhere (e.g. a
// caller that also reads Metrics fields directly, like Reconciler) must use
// NewRecorderFromMetrics instead — calling this with the same registry a
// Metrics bundle already lives on panics on the second, colliding
// MustRegister call.
func NewRecorder(registry *prometheus.Registry, eventRecorder record.EventRecorder, node string) *Recorder {
	return NewRecorderFromMetrics(NewMetrics(registry), eventRecorder, node)
}

// NewRecorderFromMetrics builds a Recorder that reports as node, reusing
// metrics (an already-registered Metrics bundle, typically built once via
// NewMetrics and also handed to a caller like Reconciler that reads its
// fields directly) rather than registering a second bundle on a second
// registry. This is the seam that lets a process expose exactly one
// Prometheus registry: two independently-registered bundles merged only at
// scrape time via prometheus.Gatherers still share one failure mode if they
// ever define an overlapping series — Gather() fails atomically for the
// whole scrape, not just the colliding family — so this operator keeps a
// single registry, and this function is how a second caller (the Applier
// path) reuses the one Metrics bundle a first caller (Lifecycle, for
// Reconciler) already registered, instead of building its own.
func NewRecorderFromMetrics(metrics *Metrics, eventRecorder record.EventRecorder, node string) *Recorder {
	if metrics == nil {
		panic("observe: NewRecorderFromMetrics: metrics must not be nil")
	}
	if node == "" {
		panic("observe: NewRecorderFromMetrics: node must not be empty")
	}
	return &Recorder{
		metrics: metrics,
		events:  NewEventRecorder(eventRecorder),
		node:    node,
	}
}

// Applied records a tier-apply attempt that applied a tier to pod's cgroup
// knob, together with its paired Event (ReasonTierApplied).
func (r *Recorder) Applied(pod *corev1.Pod, knob, result, reason string) {
	r.record(pod, knob, result, reason, r.events.TierApplied)
}

// Reverted records a tier-apply attempt that removed a previously applied
// tier from pod's cgroup knob, together with its paired Event
// (ReasonTierReverted).
func (r *Recorder) Reverted(pod *corev1.Pod, knob, result, reason string) {
	r.record(pod, knob, result, reason, r.events.TierReverted)
}

// Inactive records a tier-apply attempt that was a deliberate no-op (e.g.
// burst requested without limits.cpu, AC-4), together with its paired Event
// (ReasonTierInactive).
func (r *Recorder) Inactive(pod *corev1.Pod, knob, result, reason string) {
	r.record(pod, knob, result, reason, r.events.TierInactive)
}

// Rejected records a tier-apply attempt that the kernel or the environment
// gate refused, together with its paired Event (ReasonWriteRejected).
func (r *Recorder) Rejected(pod *corev1.Pod, knob, result, reason string) {
	r.record(pod, knob, result, reason, r.events.WriteRejected)
}

// IdleSuppressed records a node-guard cgroup change and its paired Event.
func (r *Recorder) IdleSuppressed(pod *corev1.Pod, knob string) {
	r.record(pod, knob, string(TierApplyResultApplied), string(TierApplyReasonNodeGuard), r.events.IdleSuppressed)
}

// IdleRestored records removal of a node-guard suppression and its paired Event.
func (r *Recorder) IdleRestored(pod *corev1.Pod, knob string) {
	r.record(pod, knob, string(TierApplyResultReverted), string(TierApplyReasonNodeGuard), r.events.IdleRestored)
}

// TierApplyResult is this package's fixed, bounded vocabulary for the
// cpu_tier_apply_total counter's result label (HC-5), modeled on
// envgate.Reason. record normalizes any caller-supplied result string
// outside this set to TierApplyResultOther before it reaches
// WithLabelValues.
type TierApplyResult string

const (
	// TierApplyResultApplied is Recorder.Applied's outcome.
	TierApplyResultApplied TierApplyResult = "applied"
	// TierApplyResultReverted is Recorder.Reverted's outcome.
	TierApplyResultReverted TierApplyResult = "reverted"
	// TierApplyResultInactive is Recorder.Inactive's outcome.
	TierApplyResultInactive TierApplyResult = "inactive"
	// TierApplyResultRejected is Recorder.Rejected's outcome.
	TierApplyResultRejected TierApplyResult = "rejected"
	// TierApplyResultOther is the fallback for any result value outside
	// this vocabulary, so an unexpected caller string collapses into one
	// series instead of minting a new cpu_tier_apply_total series per
	// unique input.
	TierApplyResultOther TierApplyResult = "other"
)

// normalizeTierApplyResult maps result to TierApplyResult, collapsing
// anything outside the known vocabulary to TierApplyResultOther.
func normalizeTierApplyResult(result string) TierApplyResult {
	switch TierApplyResult(result) {
	case TierApplyResultApplied, TierApplyResultReverted, TierApplyResultInactive, TierApplyResultRejected:
		return TierApplyResult(result)
	default:
		return TierApplyResultOther
	}
}

// TierApplyReason is this package's fixed, bounded vocabulary for the
// cpu_tier_apply_total counter's reason label (HC-5), modeled on
// envgate.Reason. Its members are the actually-possible non-error and
// sentinel-error causes a tier-apply attempt can report — drawn from this
// operator's own contracts (the Event reasons in events.go, the cgroup
// package's sentinel errors, and the environment gate) — not raw kernel or
// filesystem error text, whose variety is unbounded. record normalizes any
// value outside this set to TierApplyReasonOther before it reaches
// WithLabelValues — otherwise every unique kernel error string would mint
// its own Prometheus series.
type TierApplyReason string

const (
	// TierApplyReasonOK means the tier-apply attempt needed no error
	// explanation.
	TierApplyReasonOK TierApplyReason = "ok"
	// TierApplyReasonValueUnknown mirrors ReasonTierValueUnknown: the tier
	// annotation carried a value this operator does not recognize.
	TierApplyReasonValueUnknown TierApplyReason = "value_unknown"
	// TierApplyReasonLimitsCPUMissing means a burst tier was requested on a
	// pod with no limits.cpu, so the tier cannot take effect (AC-4) — the
	// deliberate no-op behind a TierInactive Event.
	TierApplyReasonLimitsCPUMissing TierApplyReason = "limits_cpu_missing"
	// TierApplyReasonCgroupQuotaMissing means the Pod spec predicts a CPU
	// quota but the live pod cgroup still reports cpu.max as unbounded.
	TierApplyReasonCgroupQuotaMissing TierApplyReason = "cgroup_quota_missing"
	// TierApplyReasonEnvironmentUnsupported mirrors
	// ReasonEnvironmentUnsupported: the node failed the environment gate
	// (cgroup v2 unified, a recognized kubepods driver, kernel >= 5.15),
	// so no cgroup write is possible.
	TierApplyReasonEnvironmentUnsupported TierApplyReason = "environment_unsupported"
	// TierApplyReasonCgroupGone mirrors cgroup.ErrCgroupGone: the pod's
	// cgroup directory disappeared between discovering the pod and acting
	// on it — an expected race, not a bug.
	TierApplyReasonCgroupGone TierApplyReason = "cgroup_gone"
	// TierApplyReasonNotPodCgroup mirrors cgroup.ErrNotPodCgroup: the
	// write-target guard refused a path that is not an individual pod
	// cgroup.
	TierApplyReasonNotPodCgroup TierApplyReason = "not_pod_cgroup"
	// TierApplyReasonEINVAL means the kernel rejected the knob write with
	// EINVAL (e.g. cpu.weight while cpu.idle=1) — the one errno the
	// cgroup package's knob layer preserves through errors.Is so callers
	// can distinguish it from any other write failure.
	TierApplyReasonEINVAL TierApplyReason = "einval"
	// TierApplyReasonNodeGuard identifies pressure-driven suppression and
	// restoration performed by the node guard.
	TierApplyReasonNodeGuard TierApplyReason = "node_guard"
	// TierApplyReasonOther is the fallback for any reason value outside
	// this vocabulary, most commonly raw kernel or filesystem error text
	// that carries no distinguishable sentinel.
	TierApplyReasonOther TierApplyReason = "other"
)

// normalizeTierApplyReason maps reason to TierApplyReason, collapsing
// anything outside the known vocabulary to TierApplyReasonOther.
func normalizeTierApplyReason(reason string) TierApplyReason {
	switch TierApplyReason(reason) {
	case TierApplyReasonOK,
		TierApplyReasonValueUnknown,
		TierApplyReasonLimitsCPUMissing,
		TierApplyReasonCgroupQuotaMissing,
		TierApplyReasonEnvironmentUnsupported,
		TierApplyReasonCgroupGone,
		TierApplyReasonNotPodCgroup,
		TierApplyReasonEINVAL,
		TierApplyReasonNodeGuard:
		return TierApplyReason(reason)
	default:
		return TierApplyReasonOther
	}
}

// record is the single place that increments cpu_tier_apply_total and
// fires an Event: every exported Recorder method funnels through it, so
// there is no code path that does one without the other. The Event message
// carries result and reason as passed, but the counter's result/reason
// labels are normalized through TierApplyResult/TierApplyReason first — an
// unrecognized value collapses to "other" rather than minting a new
// Prometheus series per unique caller string (HC-5).
func (r *Recorder) record(pod *corev1.Pod, knob, result, reason string, fire func(pod *corev1.Pod, messageFmt string, args ...any)) {
	if pod == nil {
		panic("observe: Recorder: pod must not be nil")
	}
	qosClass := qos.ClassOf(pod.Spec)
	normalizedResult := normalizeTierApplyResult(result)
	normalizedReason := normalizeTierApplyReason(reason)
	r.metrics.TierApplyTotal.WithLabelValues(r.node, pod.Namespace, string(qosClass), string(normalizedResult), string(normalizedReason)).Inc()
	fire(pod, "%s: result=%s reason=%s", knob, result, reason)
}
