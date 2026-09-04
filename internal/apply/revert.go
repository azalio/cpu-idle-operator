package apply

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	corev1 "k8s.io/api/core/v1"

	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
	"github.com/azalio/cpu-idle-operator/internal/tier"
)

// Revert clears state's active CPU tiers from pod's cgroup, restoring
// cpu.weight along the way whenever that clears cpu.idle (BuildPlan's own
// restoreWeight decision, shared with Apply — see plan.go). Apply is the
// other path that can also restore cpu.weight now (a pod that keeps
// requesting burst while only its tier annotation is removed still clears
// cpu.idle, through Apply, not Revert); kubelet itself never touches
// cpu.weight on a live pod cgroup (resolution T-006), so between the two of
// them nothing else will.
//
// The order — cpu.idle first, cpu.weight second, cpu.max.burst last — is
// fixed by measurement, not style (resolution T-005): the kernel rejects
// any cpu.weight write with EINVAL while cpu.idle==1, for any value
// including the one already on disk. BuildPlan enforces this by
// construction (cpu.weight is only ever placed immediately after
// cpu.idle), and the write loop below stops on the first failed write, so
// a failed cpu.idle write is never followed by a cpu.weight attempt: the
// kernel would reject it anyway, and a doomed attempt would only leave a
// false trail in the metrics (CCR-1).
//
// The restored weight always comes from pod's spec as observed right now
// (qos.RestoreWeight), never a value captured when the pod entered idle
// (AC-15): Applier caches neither a weight nor a requests.cpu value
// anywhere — there is no field for it, by construction, not by discipline.
// A cache would also not survive an agent restart, while an idle pod does
// (resolution T-006).
func (a *Applier) Revert(ctx context.Context, pod *corev1.Pod, state Snapshot) error {
	if pod == nil {
		panic("apply: Applier.Revert: pod must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := cgroup.PodCgroupPath(a.cgroupRoot, a.kubepodsName, a.driver, qos.ToCgroupClass(qos.ClassOf(pod.Spec)), string(pod.UID))
	if err != nil {
		return fmt.Errorf("apply: revert: pod cgroup path: %w", err)
	}

	for _, write := range a.buildPlan(pod, tier.State{}, state) {
		if err := ctx.Err(); err != nil {
			return err
		}
		writeErr := a.writer.WriteKnob(a.cgroupRoot, a.kubepodsName, dir, write.Knob, write.Value)
		switch {
		case writeErr == nil:
			a.recordCompletedWrite(pod.UID, write)
			a.recorder.Reverted(pod, write.Knob, string(observe.TierApplyResultReverted), string(observe.TierApplyReasonOK))
		case errors.Is(writeErr, cgroup.ErrCgroupGone):
			a.forgetPendingRestore(pod.UID)
			// Intent: the pod raced to deletion mid-plan, same silent-return
			// contract Apply uses for the identical race.
			return nil
		case errors.Is(writeErr, syscall.EINVAL):
			// Intent: this is the measured failure mode this function is
			// built around. When it lands on the cpu.idle write, the loop
			// stops here and the plan's next entry (cpu.weight, per
			// buildPlan) is never attempted — INV-2 holds structurally,
			// not by a special case.
			a.recorder.Rejected(pod, write.Knob, string(observe.TierApplyResultRejected), string(observe.TierApplyReasonEINVAL))
			if write.Knob == KnobCPUWeight {
				return fmt.Errorf("apply: revert: restore %s: %w", write.Knob, writeErr)
			}
			return nil
		case errors.Is(writeErr, cgroup.ErrNotPodCgroup):
			a.recorder.Rejected(pod, write.Knob, string(observe.TierApplyResultRejected), string(observe.TierApplyReasonNotPodCgroup))
			return fmt.Errorf("apply: revert: write %s: %w", write.Knob, writeErr)
		default:
			a.recorder.Rejected(pod, write.Knob, string(observe.TierApplyResultRejected), string(observe.TierApplyReasonOther))
			return fmt.Errorf("apply: revert: write %s: %w", write.Knob, writeErr)
		}
	}
	return nil
}
