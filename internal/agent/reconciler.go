package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/azalio/cpi-idle-operator/internal/apply"
	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/observe"
	"github.com/azalio/cpi-idle-operator/internal/qos"
	"github.com/azalio/cpi-idle-operator/internal/tier"
)

// Applier is the subset of *apply.Applier Reconciler depends on: Apply
// converges a pod that requests an active tier (or that carries a Note
// tier.Desired could not resolve into one, so its Event still needs
// reporting), Revert clears an active tier, restoring cpu.weight along the
// way. Production code always wires in a
// real *apply.Applier (built by ST-007/ST-008's apply.NewApplier), which
// already owns every cgroup write, Event, and metric this package must
// not repeat (the subtask's own instruction: reuse Apply/Revert, never
// duplicate their write or tracing logic). Tests substitute a fake to
// assert call counts without touching a filesystem.
type Applier interface {
	Apply(ctx context.Context, pod *corev1.Pod) error
	Revert(ctx context.Context, pod *corev1.Pod, state apply.Snapshot) error
}

// Reconciler drives a single pod's actual cgroup state toward its
// annotation-derived desired tier state. It reads pods exclusively from
// an informer's cache (never a direct API call) and calls only the
// already-built Applier for every write.
type Reconciler struct {
	lister       corelisters.PodLister
	applier      Applier
	cgroupRoot   string
	kubepodsName string
	driver       cgroup.Driver
	metrics      *observe.Metrics
	node         string
	logger       *slog.Logger

	// podsInTierLabels is the label-set metrics.PodsInTier carried after
	// the previous refreshPodsInTier pass. It exists purely so that pass
	// can delete only the series that disappeared (a pod that changed tier
	// or left the cache) instead of calling GaugeVec.Reset -- Reset drops
	// every child immediately, and a scrape landing in the window between
	// that Reset and this pass's last Set would read zero or a partial
	// count for a pod whose tier never actually changed. This is state
	// about the metric's own previous shape, not about a pod, so it does
	// not reintroduce the per-pod cache Reconcile otherwise avoids (see
	// Reconcile's Intent comment on refreshPodsInTier). Reconcile is only
	// ever driven by Informer.Run's single-goroutine workqueue loop
	// (informer.go), so this field is written by at most one goroutine at
	// a time and needs no lock of its own.
	podsInTierLabels map[podsInTierKey]struct{}

	// lastNotes is the signature (noteSignature) of the tier.Note set most
	// recently reported for each pod this Reconciler has seen, keyed by the
	// same namespace/name key Reconcile is invoked with. It exists because
	// Apply always reports whatever notes tier.Desired currently returns
	// (Applier.reportNotes) purely because a Note was present at all, and
	// Reconcile routes a pod through Apply on every pass a Note is present
	// even when its own plan is empty (see Reconcile's routing comment on
	// wantsActiveTier || len(notes) != 0) -- without this record, a Note
	// whose underlying condition never changes (a burst request still
	// missing a CPU limit, an annotation still carrying the same
	// unrecognized value) would re-fire its Event and increment
	// cpi_tier_apply_total on every ~60s resync forever, even though
	// nothing about the pod changed between passes.
	//
	// Entries are deleted the moment a pod's notes go empty (podNoteChanged)
	// and again on every refreshPodsInTier pass for any pod that has left
	// the informer cache entirely (pruneNoteState) -- the same "recompute
	// from a full listing, then drop what no longer applies" shape
	// podsInTierLabels above already uses -- so this map cannot grow
	// forever on a node with high pod churn. It is state about what this
	// Reconciler last told the user, not a cache of pod weight, and like
	// podsInTierLabels needs no lock: Reconcile only ever runs on
	// Informer.Run's single-goroutine workqueue loop.
	lastNotes map[string]string
}

// NewReconciler builds a Reconciler that reconciles pods from lister's
// cache against their cgroup state under cgroupRoot (using kubepodsName as
// the top-level kubepods slice/directory name and driver as the cgroup
// driver to compute each pod's cgroup path), calling applier for every
// actual convergence and reporting resync-caught drift on
// metrics.ResyncDriftTotal, labeled as node.
func NewReconciler(lister corelisters.PodLister, applier Applier, cgroupRoot, kubepodsName string, driver cgroup.Driver, metrics *observe.Metrics, node string) *Reconciler {
	if lister == nil {
		panic("agent: NewReconciler: lister must not be nil")
	}
	if applier == nil {
		panic("agent: NewReconciler: applier must not be nil")
	}
	if cgroupRoot == "" {
		panic("agent: NewReconciler: cgroupRoot must not be empty")
	}
	if kubepodsName == "" {
		panic("agent: NewReconciler: kubepodsName must not be empty")
	}
	if !driver.Valid() {
		panic(fmt.Sprintf("agent: NewReconciler: unknown driver %q", driver))
	}
	if metrics == nil {
		panic("agent: NewReconciler: metrics must not be nil")
	}
	if node == "" {
		panic("agent: NewReconciler: node must not be empty")
	}
	return &Reconciler{
		lister:       lister,
		applier:      applier,
		cgroupRoot:   cgroupRoot,
		kubepodsName: kubepodsName,
		driver:       driver,
		metrics:      metrics,
		node:         node,
		logger:       slog.Default(),
	}
}

// Reconcile drives the pod identified by key toward its desired CPU-tier
// state: it fetches the pod from the informer's cache, computes the
// desired state (tier.Desired), reads the pod's cgroup Snapshot, and calls
// Applier.Apply or Applier.Revert when actual state diverges from desired,
// or when tier.Desired produced a Note (AC-4/AC-16) that must still reach
// the user even though the cgroup itself needs no write.
//
// When desired already matches actual and tier.Desired produced no Note,
// Reconcile returns before calling Apply/Revert or logging anything: zero
// writes, zero Events, and no Info-level log line (INV-6) — logging
// "nothing to do" on every pass over every pod would bury the one drift
// signal that matters in noise. A Note alone (no cgroup divergence) still
// routes through Apply so its Event fires, but skips the "reconciling"
// Info log and the resync-drift metric: neither cgroup drift nor an actual
// write happened, only something worth telling the user about.
//
// resync is true when this call was triggered by the informer's periodic
// full resync replaying an unchanged pod rather than an observed
// Add/Update/Delete event (resolution T-011): a divergence caught this way
// means some writer other than this agent touched the cgroup between
// events, so it additionally increments cpi_resync_drift_total. Callers
// outside this package's own Informer.Run loop should pass false.
//
// A pod no longer present in the cache (apierrors.IsNotFound) is treated
// exactly like apply.Applier treats cgroup.ErrCgroupGone: a silent, nil
// return, not an error — the pod was deleted between the informer handing
// the key to the queue and this call running, an expected race, and its
// cgroup directory is almost certainly gone too.
func (r *Reconciler) Reconcile(ctx context.Context, key string, resync bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Intent: recomputed from a full listing of this node's pods on every
	// call, never incremented/decremented per event — a gauge is a
	// snapshot of current state, and this Reconciler only ever sees pods
	// one at a time, so an incremental update could never recover from a
	// missed or misordered event without drifting from reality forever. A
	// full recompute is self-correcting by construction: a stale entry
	// left over from a pod that has since changed tier, or been deleted,
	// simply does not get re-Set this pass and Reset() below clears it,
	// rather than requiring every code path that could change a pod's
	// tier membership to also remember to decrement the old bucket.
	if err := r.refreshPodsInTier(); err != nil {
		r.logger.Error("agent: failed to refresh cpi_pods_in_tier", "error", err)
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("agent: reconcile: split key %q: %w", key, err)
	}

	pod, err := r.lister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("agent: reconcile: get pod %s: %w", key, err)
	}

	desired, notes := tier.Desired(pod)
	wantsActiveTier := desired.IdleRequested || desired.BurstRequested

	// Intent: resolution 14 makes the spec-derived class authoritative and
	// status.qosClass a sanity check only — this is that check's one
	// production call site. A mismatch never changes desired.QoSClass or
	// this pod's cgroup path; it is logged, once per pod per reconcile
	// pass (this call site runs exactly once per Reconcile invocation),
	// purely so a real disagreement is visible before it silently sends a
	// write to the wrong QoS class's cgroup path.
	if mismatch, message := qos.VerifyAgainstStatus(desired.QoSClass, pod.Status.QOSClass); mismatch {
		r.logger.Warn(message, "pod", key)
	}

	dir, err := cgroup.PodCgroupPath(r.cgroupRoot, r.kubepodsName, r.driver, qos.ToCgroupClass(desired.QoSClass), string(desired.UID))
	if err != nil {
		return fmt.Errorf("agent: reconcile: pod cgroup path: %w", err)
	}

	snapshot, err := apply.ReadSnapshot(dir)
	if err != nil {
		if errors.Is(err, cgroup.ErrCgroupGone) {
			return nil
		}
		return fmt.Errorf("agent: reconcile: read snapshot: %w", err)
	}

	// Intent: compare against the same target Applier itself would
	// converge toward — desired as computed, or the fully-cleared state
	// apply.Applier.Revert's own revertPlan compares against when no
	// tier is requested — reusing apply.BuildPlan (ST-007) rather than
	// re-deriving a second, potentially-divergent notion of "matches".
	// restoreWeight only matters when the plan actually clears cpu.idle,
	// which already makes the plan non-empty on its own, so it never
	// changes this call's answer to "is there anything to do" — but a
	// real computation here, not a placeholder, keeps this call
	// unsurprising if a future caller ever inspects the plan's contents
	// instead of just its length.
	target := desired
	if !wantsActiveTier {
		target = tier.State{}
	}
	plan := apply.BuildPlan(target, snapshot, qos.RestoreWeight(pod.Spec))

	// Intent: a Note is worth reporting again only the first time it
	// appears and again if it changes (a different Code, or the same Code
	// with a different Message) -- an unchanged Note repeated pass after
	// pass carries no new information, exactly the condition INV-6 already
	// treats as silence for cgroup writes. podNoteChanged compares this
	// pass's notes against the last set actually reported for this pod and
	// records the new set as a side effect regardless of which branch
	// below runs, so it must be called on every pass, not only when it
	// might change the early-return decision.
	notesChanged := r.podNoteChanged(key, notes)

	// Intent: "nothing to write" and "nothing to say" are different
	// conditions (AC-4/AC-16 — a burst request with no CPU limit, or an
	// unrecognized tier value, must still reach the user even when the
	// cgroup itself needs no write). INV-6's early return only applies
	// when both are true: a converged pod with no Note produces zero
	// writes, zero Events, and no Info log, exactly as before.
	if len(plan) == 0 && len(notes) == 0 {
		return nil
	}

	// Intent: the fix for the repeated-Event defect -- a pod with nothing
	// to write and a Note that has already been reported, unchanged, must
	// stay silent too, or Apply's unconditional reportNotes would re-fire
	// the same Event and cpi_tier_apply_total increment every resync pass
	// forever (observed on a live stand as an ever-growing TierInactive
	// count for a pod whose state never changed). A pod with real cgroup
	// work to do (len(plan) != 0) always proceeds to Apply/Revert below
	// regardless of notesChanged: the write itself must still happen.
	if len(plan) == 0 && !notesChanged {
		return nil
	}

	if resync && len(plan) != 0 {
		r.metrics.ResyncDriftTotal.WithLabelValues(r.node, pod.Namespace, string(desired.QoSClass)).Inc()
	}

	if len(plan) != 0 {
		r.logger.Info("reconciling pod cgroup tier",
			"pod", key,
			"resync", resync,
			"idle_requested", desired.IdleRequested,
			"burst_requested", desired.BurstRequested,
		)
	}

	// Intent: a Note only ever arises from tier.Desired's own annotation
	// parsing, which only Apply re-derives and reports (Applier.reportNotes)
	// — Revert never calls tier.Desired at all. So any pod carrying a Note
	// must route through Apply even when it requests no active tier (an
	// unrecognized tier value with no burst annotation present): for that
	// shape, Apply's own desired collapses to the same fully-cleared target
	// Revert would use (both IdleRequested and BurstActive stay false), so
	// routing here through Apply instead of Revert changes only which
	// function reports the outcome, never what the cgroup converges to.
	if wantsActiveTier || len(notes) != 0 {
		return r.applier.Apply(ctx, pod)
	}
	return r.applier.Revert(ctx, pod, snapshot)
}

// noteSignature encodes notes into one comparable string so podNoteChanged
// can detect any change -- a note appearing, disappearing, or changing its
// Code or Message -- with a plain string comparison instead of a deep slice
// comparison. Both Code and Message participate, not Code alone: two passes
// that both produce NoteUnknownTierValue but for different annotation
// values (the user edited a still-unrecognized tier value) carry genuinely
// different information for the user, so that counts as a change too. The
// empty string always means "no notes"; it is never produced for a
// non-empty notes slice, since every Note carries a non-empty Code.
func noteSignature(notes []tier.Note) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, note := range notes {
		b.WriteString(string(note.Code))
		b.WriteByte(0)
		b.WriteString(note.Message)
		b.WriteByte(0)
	}
	return b.String()
}

// podNoteChanged reports whether key's current notes differ from the set
// last recorded for it in r.lastNotes, then updates that record to match
// notes. The very first pass over a given key always reports a change
// (there is nothing recorded yet to compare against), which is what lets a
// Note present from a pod's first reconcile onward still reach the user
// exactly once (AC-4/AC-16) rather than being suppressed as "unchanged".
// When notes is empty, the key's entry is deleted rather than set to the
// empty signature, both to bound r.lastNotes to pods currently carrying a
// Note and so a Note that disappears and later reappears is treated as new
// again, per this fix's own requirement.
func (r *Reconciler) podNoteChanged(key string, notes []tier.Note) bool {
	signature := noteSignature(notes)
	if r.lastNotes == nil {
		r.lastNotes = make(map[string]string)
	}
	previous, seen := r.lastNotes[key]
	changed := !seen || previous != signature
	if signature == "" {
		delete(r.lastNotes, key)
	} else {
		r.lastNotes[key] = signature
	}
	return changed
}

// podsInTierKey identifies one cpi_pods_in_tier series: everything the
// metric is labeled by except node, which is constant for this Reconciler
// (every pod it ever lists lives on the same node, r.node).
type podsInTierKey struct {
	namespace string
	qosClass  string
	tier      string
}

// refreshPodsInTier recomputes metrics.PodsInTier from a full listing of
// this node's pods in the informer cache (see Reconcile's own Intent
// comment on why this is a full recompute, not an increment/decrement per
// event). "In a tier" means tier.Desired's own notion of a tier actually
// taking effect: IdleRequested for idle (idle has no separate "requested
// but inactive" case — cpu.idle either is or is not requested), and
// BurstActive — not merely BurstRequested — for burst, since a burst
// annotation with no positive CPU limit to act on is reported as
// TierInactive (AC-4), never actually reaching the burst tier.
//
// Intent: every current key is Set before any stale key is deleted, so a
// concurrent scrape never observes a window where a series that is still
// current has been wiped but not yet re-written -- the defect a prior
// Reset-then-Set-loop version of this function had. Only labels present in
// r.podsInTierLabels (this Reconciler's own previous pass) but absent from
// this pass's counts are deleted, which is exactly the set of series that
// stopped applying (a pod's tier changed, or it left the cache) -- an
// unrelated series this Reconciler never wrote is never touched.
func (r *Reconciler) refreshPodsInTier() error {
	pods, err := r.lister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("agent: refresh pods-in-tier: list pods: %w", err)
	}

	counts := make(map[podsInTierKey]float64, len(pods))
	// Intent: collected alongside counts, from the same full listing, so
	// pruneNoteState below can drop any r.lastNotes entry for a pod that
	// has left the informer cache entirely -- the leak guard podNoteChanged
	// alone cannot provide, since it only ever runs (and so only ever
	// deletes an entry) for a pod key Reconcile is actually invoked with.
	// A pod deleted while still carrying a Note-worthy annotation would
	// otherwise leave its entry in r.lastNotes forever.
	currentPodKeys := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		if podKey, err := cache.MetaNamespaceKeyFunc(pod); err == nil {
			currentPodKeys[podKey] = struct{}{}
		}
		desired, _ := tier.Desired(pod)
		if desired.IdleRequested {
			counts[podsInTierKey{namespace: pod.Namespace, qosClass: string(desired.QoSClass), tier: "idle"}]++
		}
		if desired.BurstActive {
			counts[podsInTierKey{namespace: pod.Namespace, qosClass: string(desired.QoSClass), tier: "burst"}]++
		}
	}

	for key, count := range counts {
		r.metrics.PodsInTier.WithLabelValues(r.node, key.namespace, key.qosClass, key.tier).Set(count)
	}
	for key := range r.podsInTierLabels {
		if _, stillPresent := counts[key]; !stillPresent {
			r.metrics.PodsInTier.DeleteLabelValues(r.node, key.namespace, key.qosClass, key.tier)
		}
	}

	currentLabels := make(map[podsInTierKey]struct{}, len(counts))
	for key := range counts {
		currentLabels[key] = struct{}{}
	}
	r.podsInTierLabels = currentLabels

	r.pruneNoteState(currentPodKeys)
	return nil
}

// pruneNoteState deletes every r.lastNotes entry whose pod key is absent
// from currentPodKeys -- the same "recompute from a full listing, then drop
// what no longer applies" shape refreshPodsInTier already uses for
// r.podsInTierLabels, reused here so a pod's note-tracking entry cannot
// outlive the pod itself in the informer cache. podNoteChanged's own
// delete-on-empty-notes path is not enough by itself: it only runs for a
// pod key Reconcile is actually invoked with, so a pod deleted while still
// carrying a Note (the common shape -- an annotation misconfigured for the
// pod's whole lifetime) would otherwise leave its entry behind forever,
// growing r.lastNotes without bound on a node with high pod churn.
func (r *Reconciler) pruneNoteState(currentPodKeys map[string]struct{}) {
	for key := range r.lastNotes {
		if _, stillPresent := currentPodKeys[key]; !stillPresent {
			delete(r.lastNotes, key)
		}
	}
}
