package observe

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// Reason is this operator's fixed, exhaustive vocabulary of Kubernetes
// Event reasons. Nothing outside this file's consts spells one out: a
// caller always goes through one of EventRecorder's named methods below,
// never record.EventRecorder directly with an ad hoc reason string.
type Reason string

const (
	// ReasonTierApplied fires when a CPU tier was actually applied on the
	// pod's cgroup.
	ReasonTierApplied Reason = "TierApplied"
	// ReasonTierReverted fires when a previously applied tier was removed
	// from the pod's cgroup.
	ReasonTierReverted Reason = "TierReverted"
	// ReasonTierInactive fires when a tier annotation is present but
	// cannot take effect (e.g. burst requested without limits.cpu, AC-4)
	// — a deliberate no-op, not an error.
	ReasonTierInactive Reason = "TierInactive"
	// ReasonTierValueUnknown fires when a tier annotation carries a value
	// this operator does not recognize; treated as no tier requested
	// (AC-16), never as an error.
	ReasonTierValueUnknown Reason = "TierValueUnknown"
	// ReasonEnvironmentUnsupported fires on a pod annotated for a tier on
	// a node that failed the environment gate (resolution T-009): without
	// this Event the pod's owner would believe the policy applied when it
	// silently did not.
	ReasonEnvironmentUnsupported Reason = "EnvironmentUnsupported"
	// ReasonWriteRejected fires when an attempted cgroup write was
	// refused (e.g. the kernel returned EINVAL).
	ReasonWriteRejected Reason = "WriteRejected"
)

// EventRecorder is a thin wrapper over record.EventRecorder — the same
// interface type a controller-runtime manager hands out from
// GetEventRecorderFor — that restricts every call to this package's fixed
// Reason vocabulary and types the involved object as *corev1.Pod, so a
// caller cannot raise an Event against anything else: this operator only
// ever has an opinion about the pod a tier annotation names.
type EventRecorder struct {
	recorder record.EventRecorder
}

// NewEventRecorder wraps recorder. recorder is built by whatever process
// wires this operator together — e.g. via record.NewBroadcaster with a
// client-go typed event sink — this package never constructs one itself.
func NewEventRecorder(recorder record.EventRecorder) *EventRecorder {
	if recorder == nil {
		panic("observe: NewEventRecorder: recorder must not be nil")
	}
	return &EventRecorder{recorder: recorder}
}

// emit raises exactly one Event of eventType with reason against pod,
// formatting the message the same way record.EventRecorder.Eventf does.
func (e *EventRecorder) emit(pod *corev1.Pod, eventType string, reason Reason, messageFmt string, args ...any) {
	if pod == nil {
		panic(fmt.Sprintf("observe: EventRecorder.%s: pod must not be nil", reason))
	}
	e.recorder.Eventf(pod, eventType, string(reason), messageFmt, args...)
}

// TierApplied records ReasonTierApplied against pod.
func (e *EventRecorder) TierApplied(pod *corev1.Pod, messageFmt string, args ...any) {
	e.emit(pod, corev1.EventTypeNormal, ReasonTierApplied, messageFmt, args...)
}

// TierReverted records ReasonTierReverted against pod.
func (e *EventRecorder) TierReverted(pod *corev1.Pod, messageFmt string, args ...any) {
	e.emit(pod, corev1.EventTypeNormal, ReasonTierReverted, messageFmt, args...)
}

// TierInactive records ReasonTierInactive against pod.
func (e *EventRecorder) TierInactive(pod *corev1.Pod, messageFmt string, args ...any) {
	e.emit(pod, corev1.EventTypeNormal, ReasonTierInactive, messageFmt, args...)
}

// TierValueUnknown records ReasonTierValueUnknown against pod.
func (e *EventRecorder) TierValueUnknown(pod *corev1.Pod, messageFmt string, args ...any) {
	e.emit(pod, corev1.EventTypeNormal, ReasonTierValueUnknown, messageFmt, args...)
}

// EnvironmentUnsupported records ReasonEnvironmentUnsupported against pod.
func (e *EventRecorder) EnvironmentUnsupported(pod *corev1.Pod, messageFmt string, args ...any) {
	e.emit(pod, corev1.EventTypeWarning, ReasonEnvironmentUnsupported, messageFmt, args...)
}

// WriteRejected records ReasonWriteRejected against pod.
func (e *EventRecorder) WriteRejected(pod *corev1.Pod, messageFmt string, args ...any) {
	e.emit(pod, corev1.EventTypeWarning, ReasonWriteRejected, messageFmt, args...)
}
