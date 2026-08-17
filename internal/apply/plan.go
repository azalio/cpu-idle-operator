package apply

import (
	"strconv"

	"github.com/azalio/cpi-idle-operator/internal/tier"
)

// Knob file names this package writes. They are exported so callers and
// tests can compare Write.Knob against a single source rather than
// re-spelling the cgroup file names.
const (
	// KnobCPUIdle is the cpu.idle knob file name.
	KnobCPUIdle = "cpu.idle"
	// KnobCPUWeight is the cpu.weight knob file name.
	KnobCPUWeight = "cpu.weight"
	// KnobCPUMax is the cpu.max knob file name.
	KnobCPUMax = "cpu.max"
	// KnobCPUMaxBurst is the cpu.max.burst knob file name.
	KnobCPUMaxBurst = "cpu.max.burst"
)

// Write is a single planned cgroup knob write, in the exact order Applier
// must execute it. BuildPlan returns the plan as an ordered slice — a value
// a test can inspect directly — precisely because the ordering invariant
// this package enforces (INV-7) leaves no trace in the pod's final cgroup
// state: a plan that reordered these two knobs would still converge to the
// identical final cpu.idle and cpu.max.burst values, so only an assertion
// against the sequence of writes, not the end state, can catch a
// regression here.
type Write struct {
	// Knob is the cgroup knob file name, relative to the pod cgroup
	// directory (e.g. KnobCPUIdle, KnobCPUMaxBurst).
	Knob string
	// Value is the exact string to write to Knob.
	Value string
}

// BuildPlan compares desired against actual and returns the ordered list of
// knob writes needed to converge, or nil when actual already matches
// desired.
//
// The ordering follows INV-7 (resolution T-001 point 2 — no kernel lock
// requires this, but the pod's idle-clear path later restores cpu.weight,
// and the bandwidth knob should already be at its final state by then):
// every write that turns a knob ON is planned before every write that
// turns a knob OFF, bandwidth (cpu.max.burst) before selection (cpu.idle)
// among the ON writes, selection before bandwidth among the OFF writes.
// That reduces to exactly [cpu.max.burst, cpu.idle] when both tiers are
// installed together (AC-13) and exactly [cpu.idle, cpu.max.burst] when
// both are removed together (INV-7) — the two sequences the contract names
// explicitly — while staying well-defined, if unmeasured, for the
// single-knob and mixed-direction cases the contract does not name.
//
// cpu.max.burst's target value is desired.BurstActive (tier.Desired's
// spec-level prediction: every container declares a positive CPU limit)
// narrowed by actual.HasQuota, the fact the pod cgroup's cpu.max actually
// reports right now: a burst write is only ever planned when both agree,
// since a spec that predicts a quota does not guarantee kubelet has set
// one yet. actual.Quota supplies the value — the burst annotation's own
// value is never parsed (SC-2).
//
// restoreWeight is the cpu.weight value BuildPlan writes immediately after
// any cpu.idle 1->0 write it plans, before cpu.max.burst (INV-2: the kernel
// physically rejects a cpu.weight write, for any value, while cpu.idle
// still reads 1, so the weight write can never precede the idle write that
// clears it). The kernel resets cpu.weight to its own default the instant
// that specific transition happens, no matter which caller drove it — a
// pod whose annotations still request burst while its tier annotation is
// removed (routed through Apply), a pod that clears every tier at once
// (routed through Revert), or --revert-all's one-shot pass, alike — so the
// decision belongs here, once, rather than re-derived by every caller that
// might plan an idle-clearing write: a second, independently-written
// caller-side check is exactly the kind of seam a future caller could get
// wrong again. Callers compute restoreWeight via qos.RestoreWeight(pod.Spec)
// from the pod's live spec, never a cached value (AC-15); it is simply
// unused whenever no cpu.idle 1->0 write is planned.
func BuildPlan(desired tier.State, actual Snapshot, restoreWeight uint64) []Write {
	burstWrite, burstOn, burstNeeded := planBurst(desired, actual)
	idleWrite, idleNeeded := planIdle(desired, actual)

	plan := make([]Write, 0, 3)
	if burstNeeded && burstOn {
		plan = append(plan, burstWrite)
	}
	if idleNeeded {
		plan = append(plan, idleWrite)
		if !desired.IdleRequested {
			plan = append(plan, Write{Knob: KnobCPUWeight, Value: strconv.FormatUint(restoreWeight, 10)})
		}
	}
	if burstNeeded && !burstOn {
		plan = append(plan, burstWrite)
	}
	return plan
}

// planBurst decides whether cpu.max.burst needs a write, and if so whether
// it belongs on the ON side of the ordering (a positive target — burst is
// becoming or staying active) or the OFF side (target 0, clearing a
// previously active burst).
func planBurst(desired tier.State, actual Snapshot) (write Write, on bool, needed bool) {
	target := uint64(0)
	if desired.BurstActive && actual.HasQuota {
		target = actual.Quota
	}
	if target == actual.Burst {
		return Write{}, false, false
	}
	return Write{Knob: KnobCPUMaxBurst, Value: strconv.FormatUint(target, 10)}, target > 0, true
}

// planIdle decides whether cpu.idle needs a write and, if so, its target
// value.
func planIdle(desired tier.State, actual Snapshot) (write Write, needed bool) {
	if desired.IdleRequested == actual.IdleActive {
		return Write{}, false
	}
	value := "0"
	if desired.IdleRequested {
		value = "1"
	}
	return Write{Knob: KnobCPUIdle, Value: value}, true
}
