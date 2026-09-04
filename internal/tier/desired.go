package tier

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// Keep enough headroom below Kubernetes' 1024-byte Event note limit even
// when fmt's %q expands every rune to a ten-byte \Uxxxxxxxx escape.
const maxReportedAnnotationValueRunes = 64

// State is the CPU-tier state Desired computes for a pod: which tiers the
// pod's annotations request, and whether the burst request can actually
// take effect given the pod's CPU limits. QoSClass and UID travel alongside
// purely so a later cgroup-path computation does not need to recompute the
// former (qos.ClassOf is the single source of truth for it) or re-fetch the
// latter.
type State struct {
	// IdleRequested is true when the tier annotation (annotations.TierKey)
	// carries annotations.TierValueIdle.
	IdleRequested bool
	// BurstRequested is true when the burst annotation
	// (annotations.BurstKey) is present at all, regardless of its value:
	// the value is never parsed (SC-2), so State carries no field for it.
	BurstRequested bool
	// BurstActive is true only when BurstRequested is true and the spec
	// predicts a pod-cgroup CPU quota: either a positive pod-level CPU
	// limit exists, or every regular/init container has one. The applier
	// still verifies the live cpu.max before writing. Always false when
	// BurstRequested is false.
	BurstActive bool
	// QoSClass is the pod's Kubernetes QoS class, computed via qos.ClassOf
	// so this package never recomputes it independently.
	QoSClass qos.Class
	// UID is the pod's UID, carried alongside QoSClass for the same
	// downstream cgroup-path computation.
	UID types.UID
}

// Desired computes pod's desired CPU-tier state purely from pod's spec and
// annotations: no filesystem access, no cgroup read or write, ever.
//
// Desired's signature carries no error return, which is how its
// postcondition — every outcome resolves without an error — holds
// structurally: a condition it cannot resolve into a definite tier (an
// unrecognized tier value, or a burst request with no CPU limit to act on)
// is reported as a Note instead, for the caller to translate into an
// observe.Recorder outcome and Event.
func Desired(pod *corev1.Pod) (State, []Note) {
	if pod == nil {
		panic("tier: Desired: pod must not be nil")
	}

	state := State{
		QoSClass: qos.ClassOf(pod.Spec),
		UID:      pod.UID,
	}
	var notes []Note

	if tierValue, present := pod.Annotations[annotations.TierKey]; present {
		switch {
		case tierValue == annotations.TierValueIdle:
			state.IdleRequested = true
		case tierValue != "":
			// Intent: a non-empty value other than TierValueIdle is a tier
			// this operator does not implement yet, not a malformed
			// request — the key is reserved for future tiers. An empty
			// value falls through neither branch: it is not idle, but it
			// is also not the "unrecognized value" case the contract notes
			// (only a non-empty mismatch does), so it resolves silently to
			// no tier requested.
			//
			// That silence is a deliberate choice, not the same defect class as
			// a false-positive cpu.max quota prediction:
			// that bug made Desired assert a request had taken effect
			// (BurstActive=true) when kubelet would not actually apply it -- a
			// false claim of success reported as if it were true. Here,
			// IdleRequested simply stays false, which matches reality: an empty
			// value carries no identifiable tier request, so it collapses to
			// the same observable outcome as the key being absent altogether
			// (see nil_annotations_map_behaves_like_absent_key in
			// desired_test.go) rather than to a request that silently fails to
			// apply. Nothing false is asserted, so no note is warranted.
			notes = append(notes, Note{
				Code:    NoteUnknownTierValue,
				Message: fmt.Sprintf("annotation %s carries unrecognized value %q; no tier requested", annotations.TierKey, reportedAnnotationValue(tierValue)),
			})
		}
	}

	if _, present := pod.Annotations[annotations.BurstKey]; present {
		state.BurstRequested = true
		if qos.HasCPUQuota(pod.Spec) {
			state.BurstActive = true
		} else {
			// Intent: without a positive limits.cpu, kubelet leaves the pod
			// cgroup's cpu.max quota unset, so cpu.max.burst has nothing to
			// act on — a deliberate no-op, not an error.
			notes = append(notes, Note{
				Code:    NoteNoCPULimit,
				Message: fmt.Sprintf("annotation %s present but pod has no positive CPU limit; burst tier is inactive", annotations.BurstKey),
			})
		}
	}

	return state, notes
}

func reportedAnnotationValue(value string) string {
	runes := []rune(value)
	if len(runes) <= maxReportedAnnotationValueRunes {
		return value
	}
	return string(runes[:maxReportedAnnotationValueRunes]) + "…"
}
