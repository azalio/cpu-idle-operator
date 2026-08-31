package tier

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

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
	// BurstActive is true only when BurstRequested is true and every
	// container in the pod — every regular container, native sidecar, and
	// plain init container alike — has a positive CPU limit — the exact
	// condition under which kubelet actually sets a cpu.max quota for the
	// pod cgroup, per hasPositiveCPULimit's doc comment. Always false when
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
			// the cpu.max.burst bug fixed alongside it in hasPositiveCPULimit:
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
				Message: fmt.Sprintf("annotation %s carries unrecognized value %q; no tier requested", annotations.TierKey, tierValue),
			})
		}
	}

	if _, present := pod.Annotations[annotations.BurstKey]; present {
		state.BurstRequested = true
		if hasPositiveCPULimit(pod.Spec) {
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

// hasPositiveCPULimit reports whether every container in the pod — every
// regular container, native sidecar, and plain init container alike —
// declares a strictly positive CPU limit, mirroring kubelet's own
// cpuLimitsDeclared check (k8s.io/kubernetes/pkg/kubelet/cm/helpers_linux.go,
// ResourceConfigForPod): a single container missing limits.cpu disables the
// pod cgroup's CPU quota entirely, so kubelet leaves cpu.max at "max"
// (unbounded) instead of summing whatever partial limits exist — there is
// then nothing for cpu.max.burst to act on. This applies uniformly to plain
// (non-restartable) init containers too: the pod cgroup's cpu.max quota is
// computed once, when the pod cgroup is created, and is never recomputed
// after the init phase completes — a plain init container without a limit
// blocks the quota exactly like a regular container without one would, even
// though the init container itself has already exited by the time the
// regular containers run.
//
// Measured on a live stand (kernel 6.17, k8s 1.36.3): a main container with
// limits.cpu 200m plus a plain init container with no CPU limit produces
// pod cgroup cpu.max "max 100000" — no quota — even though the main
// container alone would otherwise qualify. (An earlier version of this
// function assumed the missing-quota condition depended on steady-state
// container coexistence and exempted plain init containers on that
// reasoning; the stand measurement disproves that reasoning, so no
// container class is exempted here.)
func hasPositiveCPULimit(spec corev1.PodSpec) bool {
	for _, container := range spec.Containers {
		if !containerHasPositiveCPULimit(container) {
			return false
		}
	}
	for _, container := range spec.InitContainers {
		if !containerHasPositiveCPULimit(container) {
			return false
		}
	}
	return true
}

// containerHasPositiveCPULimit reports whether container declares a
// strictly positive limits.cpu.
func containerHasPositiveCPULimit(container corev1.Container) bool {
	quantity, ok := container.Resources.Limits[corev1.ResourceCPU]
	return ok && quantity.Sign() > 0
}
