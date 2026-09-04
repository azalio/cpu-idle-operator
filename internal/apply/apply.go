// Package apply reconciles a single pod's actual cgroup state toward its
// desired CPU-tier state: it snapshots the pod cgroup's knob files once,
// builds an ordered plan of writes (INV-7), executes the plan through a
// Writer, and reports every write it actually executes through
// observe.Recorder — never a write it only planned or a write the guard in
// internal/cgroup rejected before it reached the kernel (CCR-1).
package apply

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
	"github.com/azalio/cpu-idle-operator/internal/tier"
)

// Writer is the cgroup knob-write operation Applier depends on. Its method
// signature matches cgroup.WriteKnob exactly (root, kubepodsName, dir, name,
// value). It exists as an interface — Applier never calls cgroup.WriteKnob
// directly — so a test can substitute a journaling fake and assert on the
// *sequence* of calls (AC-13), which asserting against a real filesystem's
// final contents cannot expose no matter what order the plan executed them
// in.
type Writer interface {
	WriteKnob(root, kubepodsName, dir, name, value string) error
}

// cgroupWriter adapts the cgroup package's free WriteKnob function to the
// Writer interface; it is Applier's writer in production.
type cgroupWriter struct{}

func (cgroupWriter) WriteKnob(root, kubepodsName, dir, name, value string) error {
	return cgroup.WriteKnob(root, kubepodsName, dir, name, value)
}

// Applier applies and reverts CPU tiers on a single pod's cgroup.
type Applier struct {
	cgroupRoot   string
	kubepodsName string
	driver       cgroup.Driver
	writer       Writer
	recorder     *observe.Recorder
	// events fires the annotation-level Events that are not a knob-write
	// outcome at all (an unrecognized tier value — AC-16), so they must not
	// go through recorder's write-outcome pairing (CCR-1 pairs a metric
	// increment with every Event recorder emits; an unrecognized tier
	// value produces zero writes, so it must produce zero counter
	// increments too, only the Event.) It is built from the same
	// underlying record.EventRecorder recorder itself wraps.
	events *observe.EventRecorder

	// pendingRestores is an in-process transaction journal, not a cached
	// weight. A UID enters only after this Applier successfully writes
	// cpu.idle=0 and leaves after the following cpu.weight write succeeds.
	// The retry always recomputes the target from the current Pod spec. It is
	// intentionally not persisted: after a process restart an arbitrary
	// cpu.weight=100 cannot safely be attributed to this operator, so the
	// operator preserves it instead of overwriting another actor's value.
	pendingRestores map[types.UID]struct{}
}

// NewApplier builds an Applier that writes cgroup knobs under cgroupRoot
// (using kubepodsName as the top-level kubepods slice/directory name and
// driver as the cgroup driver), reporting write outcomes through recorder
// and annotation-level notices through events. cgroupRoot, kubepodsName and
// driver come from the agent's configuration and the environment gate's
// decision respectively — Applier never detects any of them itself.
func NewApplier(cgroupRoot, kubepodsName string, driver cgroup.Driver, recorder *observe.Recorder, events *observe.EventRecorder) *Applier {
	if cgroupRoot == "" {
		panic("apply: NewApplier: cgroupRoot must not be empty")
	}
	if kubepodsName == "" {
		panic("apply: NewApplier: kubepodsName must not be empty")
	}
	if !driver.Valid() {
		panic(fmt.Sprintf("apply: NewApplier: unknown driver %q", driver))
	}
	if recorder == nil {
		panic("apply: NewApplier: recorder must not be nil")
	}
	if events == nil {
		panic("apply: NewApplier: events must not be nil")
	}
	return &Applier{
		cgroupRoot:      cgroupRoot,
		kubepodsName:    kubepodsName,
		driver:          driver,
		writer:          cgroupWriter{},
		recorder:        recorder,
		events:          events,
		pendingRestores: make(map[types.UID]struct{}),
	}
}

// Apply reconciles pod's actual cgroup state toward its desired tier state:
// it computes the desired state from pod's annotations and spec
// (tier.Desired), reads a Snapshot exactly once, builds a plan (BuildPlan),
// and executes each planned write in order, reporting every write it
// actually executes to observe.Recorder. reportNotices lets the serialized
// Reconciler suppress annotation-level Events it has already published for
// the same Pod UID and note signature; it never suppresses write outcomes.
//
// Two silences are deliberately different (AC-16). An unrecognized tier
// value (tier.NoteUnknownTierValue) produces zero writes and exactly one
// Event (TierValueUnknown) — no counter increment, since no write was even
// attempted. A pod cgroup that has disappeared (cgroup.ErrCgroupGone)
// produces zero writes, zero Events, and a nil error here; the reconciler's
// post-apply read separately retries when the cached Pod is still Running.
func (a *Applier) Apply(ctx context.Context, pod *corev1.Pod, reportNotices bool) error {
	if pod == nil {
		panic("apply: Applier.Apply: pod must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	desired, notes := tier.Desired(pod)

	dir, err := cgroup.PodCgroupPath(a.cgroupRoot, a.kubepodsName, a.driver, qos.ToCgroupClass(desired.QoSClass), string(desired.UID))
	if err != nil {
		return fmt.Errorf("apply: pod cgroup path: %w", err)
	}

	snapshot, err := ReadSnapshot(dir)
	if err != nil {
		if errors.Is(err, cgroup.ErrCgroupGone) {
			a.forgetPendingRestore(pod.UID)
			return nil
		}
		return fmt.Errorf("apply: read snapshot: %w", err)
	}
	plan := a.buildPlan(pod, desired, snapshot)
	for _, write := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		writeErr := a.writer.WriteKnob(a.cgroupRoot, a.kubepodsName, dir, write.Knob, write.Value)
		switch {
		case writeErr == nil:
			a.recordCompletedWrite(pod.UID, write)
			a.reportSuccess(pod, write)
		case errors.Is(writeErr, cgroup.ErrCgroupGone):
			a.forgetPendingRestore(pod.UID)
			// Intent: the pod raced to deletion mid-plan. The remaining
			// writes in this plan target a cgroup that no longer exists,
			// so stop here rather than attempt them against nothing —
			// same silent-return contract as the Snapshot-read race.
			return nil
		case errors.Is(writeErr, syscall.EINVAL):
			// Intent: the kernel rejected this value (e.g. lowering
			// cpu.max below an already-set cpu.max.burst). Recorded once,
			// not retried in this pass — a fresh reconcile with a fresh
			// Snapshot may find the condition resolved, but hammering the
			// same write again immediately cannot succeed.
			a.recorder.Rejected(pod, write.Knob, string(observe.TierApplyResultRejected), string(observe.TierApplyReasonEINVAL))
			if write.Knob == KnobCPUWeight {
				return fmt.Errorf("apply: restore %s: %w", write.Knob, writeErr)
			}
			if reportNotices {
				a.reportApplyNotices(pod, desired, notes, snapshot)
			}
			return nil
		case errors.Is(writeErr, cgroup.ErrNotPodCgroup):
			a.recorder.Rejected(pod, write.Knob, string(observe.TierApplyResultRejected), string(observe.TierApplyReasonNotPodCgroup))
			return fmt.Errorf("apply: write %s: %w", write.Knob, writeErr)
		default:
			a.recorder.Rejected(pod, write.Knob, string(observe.TierApplyResultRejected), string(observe.TierApplyReasonOther))
			return fmt.Errorf("apply: write %s: %w", write.Knob, writeErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Commit annotation notices only after every retryable cgroup operation
	// has succeeded. Reconciler records the matching de-duplication signature
	// only when Apply returns nil; emitting a notice before a later write
	// failed would therefore emit it again on every retry.
	if reportNotices {
		a.reportApplyNotices(pod, desired, notes, snapshot)
	}
	return nil
}

// NeedsWeightRepair reports whether this process completed cpu.idle=0 for
// pod but has not yet completed the paired cpu.weight restore. Weight 100
// (or any other value) without this ownership evidence is never enough.
func (a *Applier) NeedsWeightRepair(pod *corev1.Pod, snapshot Snapshot) bool {
	if pod == nil || snapshot.IdleActive || a.pendingRestores == nil {
		return false
	}
	if _, pending := a.pendingRestores[pod.UID]; !pending {
		return false
	}
	if snapshot.Weight == qos.RestoreWeight(pod.Spec) {
		a.forgetPendingRestore(pod.UID)
		return false
	}
	return true
}

// ForgetPod discards process-local transaction state after the reconciler
// confirms that a Pod or its cgroup no longer exists. Without this explicit
// deletion edge, a Pod removed after cpu.idle cleared but before cpu.weight
// restored would leave one UID in the journal for the lifetime of the agent.
func (a *Applier) ForgetPod(uid types.UID) {
	a.forgetPendingRestore(uid)
}

func (a *Applier) buildPlan(pod *corev1.Pod, desired tier.State, snapshot Snapshot) []Write {
	weight := qos.RestoreWeight(pod.Spec)
	plan := BuildPlan(desired, snapshot, weight)
	if !a.NeedsWeightRepair(pod, snapshot) {
		return plan
	}
	// Finish an interrupted idle-off transaction before any new tier write.
	// In particular, if the annotation was re-added between retries,
	// cpu.weight must be repaired while cpu.idle is still 0: after the
	// planned cpu.idle=1 write the kernel would reject that repair.
	repair := Write{Knob: KnobCPUWeight, Value: strconv.FormatUint(weight, 10)}
	return append([]Write{repair}, plan...)
}

func (a *Applier) recordCompletedWrite(uid types.UID, write Write) {
	if write.Knob == KnobCPUIdle && write.Value == "0" {
		if a.pendingRestores == nil {
			a.pendingRestores = make(map[types.UID]struct{})
		}
		a.pendingRestores[uid] = struct{}{}
		return
	}
	if write.Knob == KnobCPUWeight {
		a.forgetPendingRestore(uid)
	}
}

func (a *Applier) forgetPendingRestore(uid types.UID) {
	delete(a.pendingRestores, uid)
}

// reportNotes translates every tier.Note Desired produced into its matching
// observe outcome: NoteNoCPULimit (a tier-apply attempt that is a
// deliberate no-op, AC-4) goes through recorder.Inactive so it carries the
// paired metric increment CCR-1 requires for a reported apply outcome;
// NoteUnknownTierValue is not an apply outcome at all — no knob was even
// considered for a write — so it goes straight to events, no counter.
func (a *Applier) reportNotes(pod *corev1.Pod, notes []tier.Note) {
	for _, note := range notes {
		switch note.Code {
		case tier.NoteUnknownTierValue:
			a.events.TierValueUnknown(pod, "%s", note.Message)
		case tier.NoteNoCPULimit:
			a.recorder.Inactive(pod, KnobCPUMaxBurst, string(observe.TierApplyResultInactive), string(note.Code))
		}
	}
}

func (a *Applier) reportApplyNotices(pod *corev1.Pod, desired tier.State, notes []tier.Note, snapshot Snapshot) {
	a.reportNotes(pod, notes)
	if desired.BurstRequested && desired.BurstActive && !snapshot.HasQuota {
		a.recorder.Inactive(pod, KnobCPUMaxBurst, string(observe.TierApplyResultInactive), string(observe.TierApplyReasonCgroupQuotaMissing))
	}
}

// reportSuccess records a write Apply actually executed successfully.
// writeIsInstall tells Applied (the knob turned on) apart from Reverted
// (the knob turned off), matching Recorder's own outcome vocabulary.
func (a *Applier) reportSuccess(pod *corev1.Pod, write Write) {
	if writeIsInstall(write) {
		a.recorder.Applied(pod, write.Knob, string(observe.TierApplyResultApplied), string(observe.TierApplyReasonOK))
		return
	}
	a.recorder.Reverted(pod, write.Knob, string(observe.TierApplyResultReverted), string(observe.TierApplyReasonOK))
}

// writeIsInstall reports whether write turns its knob on rather than off.
// cpu.idle and cpu.max.burst both use "0" as their off value, so a non-"0"
// value on either one is always an install. cpu.weight is never an
// install, regardless of its value: BuildPlan only ever plans a
// cpu.weight write immediately after a cpu.idle 1->0 write, restoring the
// value the kernel just reset — the same event Revert always reports as
// Reverted, so Apply must classify it identically rather than let a
// weight value (never "0" in practice) fall through to the ON branch.
func writeIsInstall(write Write) bool {
	if write.Knob == KnobCPUWeight {
		return false
	}
	return write.Value != "0"
}
