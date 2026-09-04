package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

const guardStateVersion = 1

var errOwnershipMarkerChanged = errors.New("guard: ownership marker changed")

// persistedState is the durable ownership record stored on a Pod. Both
// values are recorded so cleanup can distinguish this guard's suppression
// from a newer value written by kubelet or another owner.
type persistedState struct {
	Version    int    `json:"version"`
	Knob       string `json:"knob"`
	Restore    string `json:"restore"`
	Suppressed string `json:"suppressed"`
}

type ownedState struct {
	pod     *corev1.Pod
	state   persistedState
	trusted bool
}

// Recover attempts to restore every suppression carrying this operator's
// ownership marker. The method remains callable while the guard is disabled
// so the explicit --revert-all path can clean up before configuration changes;
// the normal Lifecycle invokes it only for an enabled guard because Pod
// annotations are tenant-controlled. An untrusted marker that would remove an
// expected kubelet quota fails closed and remains for operator investigation.
func (g *Guard) Recover(ctx context.Context) error {
	pods, err := g.lister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("guard: recover: list pods: %w", err)
	}

	seen := make(map[string]struct{}, len(pods))
	var errs []error
	for _, pod := range pods {
		uid := string(pod.UID)
		seen[uid] = struct{}{}
		state, marked, stateErr := g.stateForPod(pod)
		if stateErr != nil {
			g.logger.Warn("guard: discarding invalid ownership marker", "pod", pod.Namespace+"/"+pod.Name, "error", stateErr)
			if clearErr := g.clearInvalidMarker(ctx, pod, pod.Annotations[annotations.GuardStateKey]); clearErr != nil {
				errs = append(errs, clearErr)
			}
			continue
		}
		if !marked {
			continue
		}
		if _, restoreErr := g.restorePod(ctx, pod, state); restoreErr != nil {
			errs = append(errs, restoreErr)
		}
	}

	// A marker PATCH may not have reached the informer cache yet. Local
	// ownership closes that window during graceful shutdown and deletion.
	for uid, owned := range g.owned {
		if _, ok := seen[uid]; ok {
			continue
		}
		if _, restoreErr := g.restorePod(ctx, owned.pod, owned.state); restoreErr != nil {
			errs = append(errs, restoreErr)
		}
	}
	return errors.Join(errs...)
}

// converge restores marked pods that are no longer desired, then suppresses
// candidates whose live pod cgroup is actually unbounded when hot. allPods
// and candidates stay separate so losing an annotation or gaining a live CPU
// quota cannot make an owned pod vanish from the cleanup set.
func (g *Guard) converge(ctx context.Context, hot bool, allPods, candidates []*corev1.Pod) error {
	candidateUIDs := make(map[string]struct{}, len(candidates))
	for _, pod := range candidates {
		candidateUIDs[string(pod.UID)] = struct{}{}
	}
	eligible := make([]*corev1.Pod, 0, len(candidates))
	restoredThisPass := make(map[string]struct{})
	seen := make(map[string]struct{}, len(allPods))
	var errs []error

	for _, pod := range allPods {
		seen[string(pod.UID)] = struct{}{}
		state, marked, stateErr := g.stateForPod(pod)
		if stateErr != nil {
			g.logger.Warn("guard: discarding invalid ownership marker", "pod", pod.Namespace+"/"+pod.Name, "error", stateErr)
			if clearErr := g.clearInvalidMarker(ctx, pod, pod.Annotations[annotations.GuardStateKey]); clearErr != nil {
				errs = append(errs, clearErr)
			}
			continue
		}
		uid := string(pod.UID)
		_, isCandidate := candidateUIDs[uid]
		isEligible := false
		if hot && isCandidate && (!marked || state.Knob == g.suppressionKnob()) {
			var eligibilityErr error
			isEligible, eligibilityErr = g.actualQuotaAllowsSuppression(pod, state, marked)
			if eligibilityErr != nil && !marked && !errors.Is(eligibilityErr, cgroup.ErrCgroupGone) {
				errs = append(errs, eligibilityErr)
			}
		}
		if marked && (!hot || !isEligible || state.Knob != g.suppressionKnob()) {
			if _, restoreErr := g.restorePod(ctx, pod, state); restoreErr != nil {
				errs = append(errs, restoreErr)
			}
			restoredThisPass[uid] = struct{}{}
			continue
		}
		if isEligible {
			eligible = append(eligible, pod)
		}
	}

	if hot {
		for _, pod := range eligible {
			if _, justRestored := restoredThisPass[string(pod.UID)]; justRestored {
				continue
			}
			if suppressErr := g.suppressPod(ctx, pod); suppressErr != nil {
				errs = append(errs, suppressErr)
			}
		}
	}
	// A Delete event removes the pod from the lister before its cgroup is
	// necessarily gone. Local ownership must not leak until process shutdown:
	// restore immediately when possible, or at least clear it once both the
	// Pod and cgroup have disappeared.
	for uid, owned := range g.owned {
		if _, present := seen[uid]; present {
			continue
		}
		if _, restoreErr := g.restorePod(ctx, owned.pod, owned.state); restoreErr != nil {
			errs = append(errs, restoreErr)
		}
	}
	return errors.Join(errs...)
}

func (g *Guard) suppressPod(ctx context.Context, pod *corev1.Pod) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	knob := g.suppressionKnob()
	desired := g.suppressedValue()
	dir, err := g.podDir(pod)
	if err != nil {
		return err
	}
	current, err := cgroup.ReadKnob(dir, knob)
	if err != nil {
		return err
	}

	state, marked, err := g.stateForPod(pod)
	if err != nil {
		return err
	}
	if !marked {
		if !isUnboundedCPUMax(current) {
			// Eligibility is determined from the actual pod cgroup. A Pod
			// spec can lag kubelet or quota enforcement can be disabled;
			// neither case permits overwriting a finite live quota.
			return nil
		}
		if current == desired {
			// This process did not create the state, so it must not claim or
			// later undo another writer's choice.
			return nil
		}
		state = persistedState{
			Version:    guardStateVersion,
			Knob:       knob,
			Restore:    current,
			Suppressed: desired,
		}
		if err := validateState(state); err != nil {
			return fmt.Errorf("guard: refuse to persist unsafe state: %w", err)
		}
		if err := g.setMarker(ctx, pod, state); err != nil {
			return err
		}
		g.owned[string(pod.UID)] = ownedState{pod: pod.DeepCopy(), state: state, trusted: true}
	} else if state.Knob != knob || (current != state.Restore && current != state.Suppressed) {
		// A newer writer changed cpu.max after the marker was created.
		// converge will relinquish ownership; never overwrite that value
		// from this defensive inner layer either.
		return nil
	}

	if current == state.Suppressed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cgroup.WriteKnob(g.cfg.CgroupRoot, g.cfg.KubepodsName, dir, state.Knob, state.Suppressed); err != nil {
		g.reportRejected(pod, state.Knob, err)
		return fmt.Errorf("guard: suppress %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	g.recorder.IdleSuppressed(pod, state.Knob)
	g.logger.Info("guard suppressed idle-tier CPU", "pod", pod.Namespace+"/"+pod.Name, "knob", state.Knob, "value", state.Suppressed)
	return nil
}

// actualQuotaAllowsSuppression reports whether pod's live cpu.max is
// unbounded. For a marked pod, the current floor still counts as eligible
// only while it exactly matches this guard's recorded transition; any third
// value belongs to kubelet or another writer and makes the pod ineligible.
func (g *Guard) actualQuotaAllowsSuppression(pod *corev1.Pod, state persistedState, marked bool) (bool, error) {
	dir, err := g.podDir(pod)
	if err != nil {
		return false, err
	}
	current, err := cgroup.ReadKnob(dir, "cpu.max")
	if err != nil {
		return false, err
	}
	if !validCPUMax(current, true) {
		return false, fmt.Errorf("guard: invalid live cpu.max %q on %s/%s", current, pod.Namespace, pod.Name)
	}
	if !marked {
		return isUnboundedCPUMax(current), nil
	}
	if state.Knob != "cpu.max" || !isUnboundedCPUMax(state.Restore) {
		return false, nil
	}
	return current == state.Restore || current == state.Suppressed, nil
}

// restorePod writes the old value only while the knob still carries the
// suppression value this guard recorded. A third value belongs to a newer
// writer (typically kubelet after a resource update), so cleanup relinquishes
// ownership without overwriting it.
func (g *Guard) restorePod(ctx context.Context, pod *corev1.Pod, state persistedState) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateState(state); err != nil {
		return false, fmt.Errorf("guard: invalid persisted state on %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	dir, err := g.podDir(pod)
	if err != nil {
		return false, err
	}
	current, err := cgroup.ReadKnob(dir, state.Knob)
	if err != nil {
		if errors.Is(err, cgroup.ErrCgroupGone) {
			if clearErr := g.clearMarker(ctx, pod, state); clearErr != nil {
				g.forgetConflictedOwnership(pod, clearErr)
				return false, clearErr
			}
			delete(g.owned, string(pod.UID))
			return false, nil
		}
		return false, err
	}

	changed := false
	if current == state.Suppressed {
		if !g.mayRestoreUnbounded(pod, state) {
			// Pod annotations are tenant-controlled metadata, not a trusted
			// ownership database. A forged or stale marker must never turn a
			// kubelet-limited Pod's finite cpu.max into "max" after restart.
			// Keep the marker and fail recovery visibly: clearing it would
			// silently strand the floor and destroy the evidence needed for
			// an operator to resolve the ambiguous state.
			return false, fmt.Errorf("guard: refuse untrusted restore of %s/%s %s from %q to unbounded %q: pod spec expects a CPU quota",
				pod.Namespace, pod.Name, state.Knob, current, state.Restore)
		}
		// A transition made by this process is trusted even if the Pod spec
		// later gains a limit: kubelet may have quota enforcement disabled or
		// not have applied a resize yet, and abandoning ownership would strand
		// the guard floor. Across restart, only a Pod that still expects no
		// kubelet quota may restore an unbounded value.
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := cgroup.WriteKnob(g.cfg.CgroupRoot, g.cfg.KubepodsName, dir, state.Knob, state.Restore); err != nil {
			g.reportRejected(pod, state.Knob, err)
			return false, fmt.Errorf("guard: restore %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		changed = true
		g.recorder.IdleRestored(pod, state.Knob)
		g.logger.Info("guard restored idle-tier CPU", "pod", pod.Namespace+"/"+pod.Name, "knob", state.Knob, "value", state.Restore)
	} else if current != state.Restore {
		g.logger.Info("guard relinquished ownership after external knob update",
			"pod", pod.Namespace+"/"+pod.Name, "knob", state.Knob,
			"guard_value", state.Suppressed, "current_value", current)
	}

	if err := g.clearMarker(ctx, pod, state); err != nil {
		g.forgetConflictedOwnership(pod, err)
		return changed, err
	}
	delete(g.owned, string(pod.UID))
	return changed, nil
}

// forgetConflictedOwnership stops an obsolete in-process transition from
// shadowing the replacement marker observed by patchMarker. Transient API
// failures deliberately retain ownership so marker cleanup can be retried.
func (g *Guard) forgetConflictedOwnership(pod *corev1.Pod, err error) {
	if errors.Is(err, errOwnershipMarkerChanged) {
		delete(g.owned, string(pod.UID))
	}
}

func (g *Guard) mayRestoreUnbounded(pod *corev1.Pod, state persistedState) bool {
	if !qos.HasCPUQuota(pod.Spec) {
		return true
	}
	owned, ok := g.owned[string(pod.UID)]
	return ok && owned.trusted && owned.state == state
}

func (g *Guard) stateForPod(pod *corev1.Pod) (persistedState, bool, error) {
	uid := string(pod.UID)
	if owned, ok := g.owned[uid]; ok {
		// Local ownership closes informer lag and remains authoritative if
		// another actor edits the internal marker while this process is
		// alive. Cleanup must use the transition this process actually made,
		// never replacement data supplied after the privileged write.
		return owned.state, true, nil
	}
	if raw := pod.Annotations[annotations.GuardStateKey]; raw != "" {
		var state persistedState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return persistedState{}, false, fmt.Errorf("decode %s: %w", annotations.GuardStateKey, err)
		}
		if err := validateState(state); err != nil {
			return persistedState{}, false, err
		}
		// Loaded markers remain untrusted and are not copied into owned.
		// That map is reserved for transitions this process created, both as
		// the trust signal and to bridge informer lag. Keeping API-loaded
		// state there would make an old cache value shadow a later informer
		// update for the lifetime of the process.
		return state, true, nil
	}
	return persistedState{}, false, nil
}

func validateState(state persistedState) error {
	if state.Version != guardStateVersion {
		return fmt.Errorf("unsupported marker version %d", state.Version)
	}
	if state.Knob != "cpu.max" {
		return fmt.Errorf("unsupported knob %q", state.Knob)
	}
	restore, restoreOK := canonicalCPUMax(state.Restore, true)
	suppressed, suppressedOK := canonicalCPUMax(state.Suppressed, false)
	if !restoreOK || !strings.HasPrefix(restore, "max ") || restore != state.Restore ||
		!suppressedOK || suppressed != state.Suppressed {
		return fmt.Errorf("invalid cpu.max transition %q -> %q", state.Restore, state.Suppressed)
	}
	return nil
}

func isUnboundedCPUMax(value string) bool {
	canonical, ok := canonicalCPUMax(value, true)
	return ok && canonical == value && strings.HasPrefix(canonical, "max ")
}

func validCPUMax(value string, allowMax bool) bool {
	canonical, ok := canonicalCPUMax(value, allowMax)
	return ok && canonical == value
}

func canonicalCPUMax(value string, allowMax bool) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return "", false
	}
	quotaField := fields[0]
	if fields[0] == "max" {
		if !allowMax {
			return "", false
		}
	} else if quota, err := strconv.ParseUint(fields[0], 10, 64); err != nil || quota < 1000 {
		return "", false
	} else {
		quotaField = strconv.FormatUint(quota, 10)
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period < 1000 || period > 1_000_000 {
		return "", false
	}
	return quotaField + " " + strconv.FormatUint(period, 10), true
}

func (g *Guard) setMarker(ctx context.Context, pod *corev1.Pod, state persistedState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("guard: encode ownership marker: %w", err)
	}
	return g.patchMarker(ctx, pod, string(raw), false, nil, "")
}

func (g *Guard) clearMarker(ctx context.Context, pod *corev1.Pod, expected persistedState) error {
	return g.patchMarker(ctx, pod, "", true, &expected, "")
}

func (g *Guard) clearInvalidMarker(ctx context.Context, pod *corev1.Pod, expectedRaw string) error {
	return g.patchMarker(ctx, pod, "", true, nil, expectedRaw)
}

// patchMarker performs an optimistic compare-and-patch against the latest
// API object. The informer copy can lag: setting must never overwrite an
// ownership record that appeared after the cached read, and clearing must
// remove only the exact state (or exact invalid bytes) the caller inspected.
func (g *Guard) patchMarker(ctx context.Context, pod *corev1.Pod, value string, clear bool, expected *persistedState, expectedRaw string) error {
	current, err := g.client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if clear {
			return nil
		}
		return cgroup.ErrCgroupGone
	}
	if err != nil {
		return fmt.Errorf("guard: get pod before ownership patch on %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if current.UID != pod.UID {
		// The old pod disappeared and its name was reused. Never attach an
		// old cgroup transition to the replacement object.
		if clear {
			return nil
		}
		return cgroup.ErrCgroupGone
	}

	currentRaw := current.Annotations[annotations.GuardStateKey]
	if clear {
		if currentRaw == "" {
			return nil
		}
		matches := false
		switch {
		case expected != nil:
			var currentState persistedState
			if json.Unmarshal([]byte(currentRaw), &currentState) == nil && validateState(currentState) == nil {
				matches = currentState == *expected
			}
		case expectedRaw != "":
			matches = currentRaw == expectedRaw
		}
		if !matches {
			return fmt.Errorf("%w on %s/%s", errOwnershipMarkerChanged, pod.Namespace, pod.Name)
		}
	} else if currentRaw != "" {
		return fmt.Errorf("%w on %s/%s", errOwnershipMarkerChanged, pod.Namespace, pod.Name)
	}

	var annotationValue any = value
	if clear {
		annotationValue = nil
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"uid": string(current.UID),
			"annotations": map[string]any{
				annotations.GuardStateKey: annotationValue,
			},
		},
	}
	if current.ResourceVersion != "" {
		patch["metadata"].(map[string]any)["resourceVersion"] = current.ResourceVersion
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("guard: encode marker patch: %w", err)
	}
	_, err = g.client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, k8stypes.MergePatchType, data, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		if clear {
			return nil
		}
		return cgroup.ErrCgroupGone
	}
	if err != nil {
		return fmt.Errorf("guard: patch ownership marker on %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

func (g *Guard) suppressionKnob() string {
	return "cpu.max"
}

func (g *Guard) suppressedValue() string {
	return g.cfg.FloorQuota
}

func (g *Guard) podDir(pod *corev1.Pod) (string, error) {
	dir, err := cgroup.PodCgroupPath(g.cfg.CgroupRoot, g.cfg.KubepodsName, g.cfg.Driver, qos.ToCgroupClass(qos.ClassOf(pod.Spec)), string(pod.UID))
	if err != nil {
		return "", fmt.Errorf("guard: pod cgroup path for %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return dir, nil
}

func (g *Guard) reportRejected(pod *corev1.Pod, knob string, err error) {
	reason := observe.TierApplyReasonOther
	switch {
	case errors.Is(err, cgroup.ErrNotPodCgroup):
		reason = observe.TierApplyReasonNotPodCgroup
	case errors.Is(err, syscall.EINVAL):
		reason = observe.TierApplyReasonEINVAL
	}
	g.recorder.Rejected(pod, knob, string(observe.TierApplyResultRejected), string(reason))
}
