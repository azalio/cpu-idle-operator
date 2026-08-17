package apply

import (
	"reflect"
	"testing"

	"github.com/azalio/cpi-idle-operator/internal/tier"
)

// TestVC2ApplyOrderIsBurstThenIdle covers VC2 [AC-13]: a pod requesting
// both tiers at once, starting from neither applied, must produce the
// sequence [cpu.max.burst, cpu.idle] — and nothing else. BuildPlan's return
// value is itself the observable artifact the contract calls for: the plan
// is asserted directly, not derived from any side effect, so a regression
// that reordered the two writes would be caught here even though it would
// converge to the identical final cgroup state.
func TestVC2ApplyOrderIsBurstThenIdle(t *testing.T) {
	t.Run("test_vc2_apply_order_is_burst_then_idle", func(t *testing.T) {
		desired := tier.State{IdleRequested: true, BurstActive: true}
		actual := Snapshot{IdleActive: false, HasQuota: true, Quota: 100000, Burst: 0}

		// restoreWeight is unused here: cpu.idle is turning ON (0->1), not
		// the 1->0 transition BuildPlan ties a weight restore to.
		got := BuildPlan(desired, actual, 0)

		want := []Write{
			{Knob: KnobCPUMaxBurst, Value: "100000"},
			{Knob: KnobCPUIdle, Value: "1"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildPlan() = %+v, want %+v", got, want)
		}
	})
}

// TestVC3RevertPlanOrderIsReversed covers VC3 [INV-7]: removing both tiers
// at once, starting from both applied, must produce the exact reverse
// sequence [cpu.idle, cpu.weight, cpu.max.burst] — cpu.idle's 1->0
// transition here also plans a cpu.weight restore (BuildPlan's own
// decision, see plan.go), spliced strictly between cpu.idle and
// cpu.max.burst per INV-2.
func TestVC3RevertPlanOrderIsReversed(t *testing.T) {
	t.Run("test_vc3_revert_plan_order_is_reversed", func(t *testing.T) {
		desired := tier.State{IdleRequested: false, BurstActive: false}
		actual := Snapshot{IdleActive: true, HasQuota: true, Quota: 100000, Burst: 100000}

		got := BuildPlan(desired, actual, 20)

		want := []Write{
			{Knob: KnobCPUIdle, Value: "0"},
			{Knob: KnobCPUWeight, Value: "20"},
			{Knob: KnobCPUMaxBurst, Value: "0"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildPlan() = %+v, want %+v", got, want)
		}
	})
}

// TestBuildPlanNoChangeIsEmpty covers the "already converged" edge case:
// actual already matches desired, so a repeated Apply on the same pod must
// plan zero writes, not a spurious no-op write of the value already on
// disk.
func TestBuildPlanNoChangeIsEmpty(t *testing.T) {
	desired := tier.State{IdleRequested: true, BurstActive: true}
	actual := Snapshot{IdleActive: true, HasQuota: true, Quota: 100000, Burst: 100000}

	got := BuildPlan(desired, actual, 0)
	if len(got) != 0 {
		t.Fatalf("BuildPlan() = %+v, want empty plan", got)
	}
}

// TestBuildPlanBurstOnlyWhenQuotaMissing covers the measured fact this
// subtask is built around: a spec that predicts a quota (BurstActive=true)
// does not by itself justify a burst write when the pod cgroup's actual
// cpu.max still reads "max" (HasQuota=false) — treating that as a numeric
// quota would compute an absurd burst value.
func TestBuildPlanBurstOnlyWhenQuotaMissing(t *testing.T) {
	desired := tier.State{BurstActive: true}
	actual := Snapshot{HasQuota: false, Burst: 0}

	got := BuildPlan(desired, actual, 0)
	if len(got) != 0 {
		t.Fatalf("BuildPlan() = %+v, want empty plan when cpu.max has no quota", got)
	}
}

// TestBuildPlanSingleKnobOnly covers installing or removing exactly one
// knob, with the other already converged: the ordering rule must not force
// a spurious write of the unchanged knob.
func TestBuildPlanSingleKnobOnly(t *testing.T) {
	t.Run("idle only", func(t *testing.T) {
		desired := tier.State{IdleRequested: true, BurstActive: false}
		actual := Snapshot{IdleActive: false, HasQuota: false, Burst: 0}

		// restoreWeight is unused here too: cpu.idle is turning ON.
		got := BuildPlan(desired, actual, 0)
		want := []Write{{Knob: KnobCPUIdle, Value: "1"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildPlan() = %+v, want %+v", got, want)
		}
	})

	t.Run("burst only", func(t *testing.T) {
		desired := tier.State{IdleRequested: false, BurstActive: true}
		actual := Snapshot{IdleActive: false, HasQuota: true, Quota: 50000, Burst: 0}

		got := BuildPlan(desired, actual, 0)
		want := []Write{{Knob: KnobCPUMaxBurst, Value: "50000"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildPlan() = %+v, want %+v", got, want)
		}
	})
}

// TestBuildPlanIdleOffRestoresWeight covers the case this subtask's seam
// defect was found in directly at BuildPlan's own level (see
// TestSeamWeightRestoredWhenOnlyTierRemoved in apply_integration_test.go
// for the same scenario through the real Applier.Apply path): cpu.idle
// clearing while cpu.max.burst stays installed must still splice a
// cpu.weight write between them, not only when every tier is cleared at
// once.
func TestBuildPlanIdleOffRestoresWeight(t *testing.T) {
	desired := tier.State{IdleRequested: false, BurstActive: true}
	actual := Snapshot{IdleActive: true, HasQuota: true, Quota: 100000, Burst: 100000}

	got := BuildPlan(desired, actual, 20)
	want := []Write{{Knob: KnobCPUIdle, Value: "0"}, {Knob: KnobCPUWeight, Value: "20"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildPlan() = %+v, want %+v", got, want)
	}
}
