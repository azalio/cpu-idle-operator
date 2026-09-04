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
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/azalio/cpu-idle-operator/internal/apply"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
	"github.com/azalio/cpu-idle-operator/internal/tier"
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
	Apply(ctx context.Context, pod *corev1.Pod, reportNotices bool) error
	Revert(ctx context.Context, pod *corev1.Pod, state apply.Snapshot) error
}

type pendingWeightRepairer interface {
	NeedsWeightRepair(pod *corev1.Pod, snapshot apply.Snapshot) bool
}

type podStateForgetter interface {
	ForgetPod(uid types.UID)
}

// Reconciler drives a single pod's actual cgroup state toward its
// annotation-derived desired tier state. It reads pods exclusively from
// an informer's cache (never a direct API call) and calls only the
// already-built Applier for every write. reportNotices is true only when the
// annotation notice set changed since its last successful publication; cgroup
// write outcome reporting remains unconditional inside apply.Applier.
type Reconciler struct {
	lister       corelisters.PodLister
	applier      Applier
	cgroupRoot   string
	kubepodsName string
	driver       cgroup.Driver
	metrics      *observe.Metrics
	node         string
	logger       *slog.Logger

	// Actual tier membership is initialized from one full-node cgroup scan,
	// then maintained per reconciled pod. Re-reading every pod cgroup on
	// every pod event would turn an informer resync into O(N^2) filesystem
	// work. Informer.Run serializes reconciliation, so these maps need no
	// lock; Prometheus collectors remain safe for concurrent scrapes.
	metricsInitialized bool
	podMemberships     map[types.UID]podTierMembership
	podUIDByKey        map[string]types.UID
	podsInTierCounts   map[podsInTierKey]float64

	// lastNotes is the signature (noteSignature) of the tier.Note set most
	// recently reported for each pod this Reconciler has seen, keyed by UID
	// so a delete/recreate under the same name is a new notification. It
	// exists because Reconcile routes a pod through Apply on every pass a
	// Note is present, even when its own plan is empty (see Reconcile's
	// routing comment on wantsActiveTier || len(notes) != 0). Apply publishes
	// those notes whenever this caller passes reportNotices=true; without
	// this record, a Note
	// whose underlying condition never changes (a burst request still
	// missing a CPU limit, an annotation still carrying the same
	// unrecognized value) would re-fire its Event and increment
	// cpu_tier_apply_total on every ~60s resync if every call enabled notice
	// reporting, even though nothing about the pod changed between passes.
	//
	// Entries are deleted the moment a pod's notices go empty (recordPodNotice)
	// and again on every refreshPodsInTier pass for any pod that has left
	// the informer cache entirely (pruneNoteState) -- the same "recompute
	// from a full listing, then drop what no longer applies" shape
	// podMemberships above already uses -- so this map cannot grow forever
	// on a node with high pod churn. It is state about what this
	// Reconciler last told the user, not a cache of pod weight, and like
	// the membership maps need no lock: Reconcile only ever runs on
	// Informer.Run's single-goroutine workqueue loop.
	lastNotes map[types.UID]string
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
// events, so it additionally increments cpu_resync_drift_total. Callers
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

	// One full scan seeds actual-state metrics and identity bookkeeping.
	// Every later event updates only its own pod, avoiding O(N^2) cgroup
	// reads across the informer's initial list and periodic resync.
	if !r.metricsInitialized {
		if err := r.refreshPodsInTier(); err != nil {
			r.logger.Error("agent: initial cpu_pods_in_tier snapshot was partial", "error", err)
		}
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("agent: reconcile: split key %q: %w", key, err)
	}

	pod, err := r.lister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.removePodByKey(key)
			return nil
		}
		return fmt.Errorf("agent: reconcile: get pod %s: %w", key, err)
	}
	r.observePodIdentity(key, pod.UID)

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
			if runningPodNeedsCgroup(pod) {
				// A Running, non-terminating pod should already have its pod
				// cgroup. Treat absence as transient/incorrect state and retry;
				// silently folding it into deletion would leave the requested
				// tier unapplied until an unrelated update or full resync.
				return fmt.Errorf("agent: reconcile: running pod %s cgroup missing: %w", key, err)
			}
			r.forgetApplierPodState(pod.UID)
			r.setPodActual(key, pod, apply.Snapshot{})
			return nil
		}
		// A transient read failure says the current membership is unknown,
		// not that both tiers are inactive. Keep the last confirmed sample;
		// replacing it with an empty Snapshot would make the gauge lie until
		// a later successful reconcile.
		return fmt.Errorf("agent: reconcile: read snapshot: %w", err)
	}
	r.setPodActual(key, pod, snapshot)

	// Intent: compare against the same target Applier itself would
	// converge toward — desired as computed, or the fully-cleared state
	// apply.Applier.Revert's own buildPlan compares against when no
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
	needsWeightRepair := false
	if !desired.IdleRequested {
		if repairer, ok := r.applier.(pendingWeightRepairer); ok {
			needsWeightRepair = repairer.NeedsWeightRepair(pod, snapshot)
		}
	}
	hasCgroupWork := len(plan) != 0 || needsWeightRepair
	actualBurstUnavailable := desired.BurstRequested && desired.BurstActive && !snapshot.HasQuota

	// Intent: a Note is worth reporting again only the first time it
	// appears and again if it changes (a different Code, or the same Code
	// with a different Message) -- an unchanged Note repeated pass after
	// pass carries no new information, exactly the condition INV-6 already
	// treats as silence for cgroup writes. podNoticeNeedsReport compares this
	// pass's notes against the last set actually reported for this pod and
	// records the new set only after the reporting path succeeds. Committing
	// it earlier would make a transient Apply failure suppress the retry and
	// lose the Event permanently.
	noticeSignature := noteSignature(notes)
	if actualBurstUnavailable {
		noticeSignature += string(observe.TierApplyReasonCgroupQuotaMissing) + "\x00"
	}
	noticesChanged := r.podNoticeNeedsReport(pod.UID, noticeSignature)
	hasNotice := len(notes) != 0 || actualBurstUnavailable

	// Intent: "nothing to write" and "nothing to say" are different
	// conditions (AC-4/AC-16 — a burst request with no CPU limit, or an
	// unrecognized tier value, must still reach the user even when the
	// cgroup itself needs no write). INV-6's early return only applies
	// when both are true: a converged pod with no Note produces zero
	// writes, zero Events, and no Info log, exactly as before.
	if !hasCgroupWork && !hasNotice {
		r.recordPodNotice(pod.UID, "")
		return nil
	}

	// Intent: the fix for the repeated-Event defect -- a pod with nothing
	// to write and a Note that has already been reported, unchanged, must
	// stay silent too. A pod with real cgroup work to do (len(plan) != 0)
	// always proceeds to Apply/Revert below regardless of noticesChanged:
	// the write itself must still happen, while reportNotices=false prevents
	// that work from re-firing the same annotation Event on every resync.
	if !hasCgroupWork && !noticesChanged {
		return nil
	}

	if resync && hasCgroupWork {
		r.metrics.ResyncDriftTotal.WithLabelValues(r.node, pod.Namespace, string(desired.QoSClass)).Inc()
	}

	if hasCgroupWork {
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
	var reconcileErr error
	if wantsActiveTier || hasNotice {
		reconcileErr = r.applier.Apply(ctx, pod, hasNotice && noticesChanged)
	} else {
		reconcileErr = r.applier.Revert(ctx, pod, snapshot)
	}
	if reconcileErr != nil {
		return errors.Join(reconcileErr, r.refreshPodActual(key, pod, dir))
	}
	r.recordPodNotice(pod.UID, noticeSignature)
	return r.refreshPodActual(key, pod, dir)
}

// noteSignature encodes notes into one comparable string so
// podNoticeNeedsReport
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

// podNoticeNeedsReport reports whether uid's current notices differ from the
// signature last successfully reported in r.lastNotes. The
// very first pass over a given UID always reports a change
// (there is nothing recorded yet to compare against), which is what lets a
// Note present from a pod's first reconcile onward still reach the user
// exactly once (AC-4/AC-16) rather than being suppressed as "unchanged".
func (r *Reconciler) podNoticeNeedsReport(uid types.UID, signature string) bool {
	previous, seen := r.lastNotes[uid]
	return !seen || previous != signature
}

// recordPodNotice commits a signature only after its reporting path succeeds.
// Empty signatures are deleted so a notice that disappears and later
// reappears is reported again, and to bound state under pod churn.
func (r *Reconciler) recordPodNotice(uid types.UID, signature string) {
	if r.lastNotes == nil {
		r.lastNotes = make(map[types.UID]string)
	}
	if signature == "" {
		delete(r.lastNotes, uid)
	} else {
		r.lastNotes[uid] = signature
	}
}

// podsInTierKey identifies one cpu_pods_in_tier series: everything the
// metric is labeled by except node, which is constant for this Reconciler
// (every pod it ever lists lives on the same node, r.node).
type podsInTierKey struct {
	namespace string
	qosClass  string
	tier      string
}

type podTierMembership struct {
	key   string
	tiers []podsInTierKey
}

// refreshPodsInTier initializes or repairs metrics.PodsInTier from a full
// listing of this node's pods in the informer cache. Production calls it
// once before incremental reconciliation; tests may call it directly to
// exercise a full repair. Membership comes from each live cgroup Snapshot,
// not annotations: cpu.idle must actually be 1, and cpu.max.burst must be
// positive under a finite cpu.max quota. This keeps the gauge truthful
// during kubelet lag, rejected writes, and external drift.
//
// Intent: every current key is Set before any stale key is deleted, so a
// concurrent scrape never observes a window where a series that is still
// current has been wiped but not yet re-written -- the defect a prior
// Reset-then-Set-loop version of this function had. Only labels present in
// r.podsInTierCounts (this Reconciler's previous state) but absent from this
// pass's counts are deleted, which is exactly the set of series that
// stopped applying (a pod's tier changed, or it left the cache) -- an
// unrelated series this Reconciler never wrote is never touched.
func (r *Reconciler) refreshPodsInTier() error {
	pods, err := r.lister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("agent: refresh pods-in-tier: list pods: %w", err)
	}

	counts := make(map[podsInTierKey]float64, len(pods))
	memberships := make(map[types.UID]podTierMembership, len(pods))
	uidsByKey := make(map[string]types.UID, len(pods))
	var refreshErrs []error
	// Intent: collected alongside counts, from the same full listing, so
	// pruneNoteState below can drop any r.lastNotes entry for a pod that
	// has left the informer cache entirely -- the leak guard recordPodNotice
	// alone cannot provide, since it only ever runs (and so only ever
	// deletes an entry) for a pod key Reconcile is actually invoked with.
	// A pod deleted while still carrying a Note-worthy annotation would
	// otherwise leave its entry in r.lastNotes forever.
	currentPodUIDs := make(map[types.UID]struct{}, len(pods))
	for _, pod := range pods {
		currentPodUIDs[pod.UID] = struct{}{}
		podKey, keyErr := cache.MetaNamespaceKeyFunc(pod)
		if keyErr != nil {
			refreshErrs = append(refreshErrs, fmt.Errorf("pod %s/%s key: %w", pod.Namespace, pod.Name, keyErr))
			continue
		}
		uidsByKey[podKey] = pod.UID
		membership := podTierMembership{key: podKey}
		qosClass := qos.ClassOf(pod.Spec)
		dir, pathErr := cgroup.PodCgroupPath(r.cgroupRoot, r.kubepodsName, r.driver, qos.ToCgroupClass(qosClass), string(pod.UID))
		if pathErr != nil {
			refreshErrs = append(refreshErrs, fmt.Errorf("pod %s/%s path: %w", pod.Namespace, pod.Name, pathErr))
			r.preserveLastKnownMembership(pod.UID, &membership, counts)
			memberships[pod.UID] = membership
			continue
		}
		snapshot, snapshotErr := apply.ReadSnapshot(dir)
		if snapshotErr != nil {
			if !errors.Is(snapshotErr, cgroup.ErrCgroupGone) || runningPodNeedsCgroup(pod) {
				refreshErrs = append(refreshErrs, fmt.Errorf("pod %s/%s snapshot: %w", pod.Namespace, pod.Name, snapshotErr))
				// A failed read makes the current state unknown. Preserve the
				// last confirmed membership instead of publishing a false
				// transition to zero; a later successful reconcile or refresh
				// will replace it.
				r.preserveLastKnownMembership(pod.UID, &membership, counts)
			}
			memberships[pod.UID] = membership
			continue
		}
		if snapshot.IdleActive {
			label := podsInTierKey{namespace: pod.Namespace, qosClass: string(qosClass), tier: "idle"}
			membership.tiers = append(membership.tiers, label)
			counts[label]++
		}
		if snapshot.HasQuota && snapshot.Burst > 0 {
			label := podsInTierKey{namespace: pod.Namespace, qosClass: string(qosClass), tier: "burst"}
			membership.tiers = append(membership.tiers, label)
			counts[label]++
		}
		memberships[pod.UID] = membership
	}

	for key, count := range counts {
		r.metrics.PodsInTier.WithLabelValues(r.node, key.namespace, key.qosClass, key.tier).Set(count)
	}
	for key := range r.podsInTierCounts {
		if _, stillPresent := counts[key]; !stillPresent {
			r.metrics.PodsInTier.DeleteLabelValues(r.node, key.namespace, key.qosClass, key.tier)
		}
	}
	for uid := range r.podMemberships {
		if _, stillPresent := currentPodUIDs[uid]; !stillPresent {
			r.forgetApplierPodState(uid)
		}
	}
	r.podMemberships = memberships
	r.podUIDByKey = uidsByKey
	r.podsInTierCounts = counts
	r.metricsInitialized = true

	r.pruneNoteState(currentPodUIDs)
	return errors.Join(refreshErrs...)
}

func (r *Reconciler) preserveLastKnownMembership(uid types.UID, membership *podTierMembership, counts map[podsInTierKey]float64) {
	previous, ok := r.podMemberships[uid]
	if !ok {
		return
	}
	membership.tiers = append(membership.tiers, previous.tiers...)
	for _, label := range previous.tiers {
		counts[label]++
	}
}

func (r *Reconciler) ensureMetricState() {
	if r.podMemberships == nil {
		r.podMemberships = make(map[types.UID]podTierMembership)
	}
	if r.podUIDByKey == nil {
		r.podUIDByKey = make(map[string]types.UID)
	}
	if r.podsInTierCounts == nil {
		r.podsInTierCounts = make(map[podsInTierKey]float64)
	}
}

func (r *Reconciler) observePodIdentity(key string, uid types.UID) {
	r.ensureMetricState()
	if previousUID, ok := r.podUIDByKey[key]; ok && previousUID != uid {
		r.forgetApplierPodState(previousUID)
		r.removePod(previousUID)
		delete(r.lastNotes, previousUID)
	}
	r.podUIDByKey[key] = uid
	if _, ok := r.podMemberships[uid]; !ok {
		r.podMemberships[uid] = podTierMembership{key: key}
	}
}

func (r *Reconciler) setPodActual(key string, pod *corev1.Pod, snapshot apply.Snapshot) {
	r.observePodIdentity(key, pod.UID)
	tiers := make([]podsInTierKey, 0, 2)
	qosClass := string(qos.ClassOf(pod.Spec))
	if snapshot.IdleActive {
		tiers = append(tiers, podsInTierKey{namespace: pod.Namespace, qosClass: qosClass, tier: "idle"})
	}
	if snapshot.HasQuota && snapshot.Burst > 0 {
		tiers = append(tiers, podsInTierKey{namespace: pod.Namespace, qosClass: qosClass, tier: "burst"})
	}
	r.replacePodMembership(pod.UID, podTierMembership{key: key, tiers: tiers})
}

func (r *Reconciler) replacePodMembership(uid types.UID, next podTierMembership) {
	r.ensureMetricState()
	touched := make(map[podsInTierKey]struct{}, 4)
	if previous, ok := r.podMemberships[uid]; ok {
		for _, label := range previous.tiers {
			r.podsInTierCounts[label]--
			touched[label] = struct{}{}
		}
		if previous.key != next.key {
			delete(r.podUIDByKey, previous.key)
		}
	}
	for _, label := range next.tiers {
		r.podsInTierCounts[label]++
		touched[label] = struct{}{}
	}
	r.podMemberships[uid] = next
	r.podUIDByKey[next.key] = uid
	r.publishTierCounts(touched)
}

func (r *Reconciler) removePodByKey(key string) {
	r.ensureMetricState()
	uid, ok := r.podUIDByKey[key]
	if !ok {
		return
	}
	r.forgetApplierPodState(uid)
	r.removePod(uid)
	delete(r.podUIDByKey, key)
	delete(r.lastNotes, uid)
}

func (r *Reconciler) forgetApplierPodState(uid types.UID) {
	if forgetter, ok := r.applier.(podStateForgetter); ok {
		forgetter.ForgetPod(uid)
	}
}

func (r *Reconciler) removePod(uid types.UID) {
	r.ensureMetricState()
	membership, ok := r.podMemberships[uid]
	if !ok {
		return
	}
	touched := make(map[podsInTierKey]struct{}, len(membership.tiers))
	for _, label := range membership.tiers {
		r.podsInTierCounts[label]--
		touched[label] = struct{}{}
	}
	delete(r.podMemberships, uid)
	delete(r.podUIDByKey, membership.key)
	r.publishTierCounts(touched)
}

func (r *Reconciler) publishTierCounts(touched map[podsInTierKey]struct{}) {
	for label := range touched {
		count := r.podsInTierCounts[label]
		if count <= 0 {
			delete(r.podsInTierCounts, label)
			r.metrics.PodsInTier.DeleteLabelValues(r.node, label.namespace, label.qosClass, label.tier)
			continue
		}
		r.metrics.PodsInTier.WithLabelValues(r.node, label.namespace, label.qosClass, label.tier).Set(count)
	}
}

func (r *Reconciler) refreshPodActual(key string, pod *corev1.Pod, dir string) error {
	snapshot, err := apply.ReadSnapshot(dir)
	if err != nil {
		if errors.Is(err, cgroup.ErrCgroupGone) {
			if runningPodNeedsCgroup(pod) {
				return fmt.Errorf("agent: refresh pod tier metrics: running pod cgroup missing: %w", err)
			}
			r.forgetApplierPodState(pod.UID)
			r.setPodActual(key, pod, apply.Snapshot{})
			return nil
		}
		// Preserve the last confirmed membership for transient failures. A
		// failed read cannot establish that an active tier disappeared.
		return fmt.Errorf("agent: refresh pod tier metrics: %w", err)
	}
	r.setPodActual(key, pod, snapshot)
	return nil
}

func runningPodNeedsCgroup(pod *corev1.Pod) bool {
	return pod != nil && pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil
}

// pruneNoteState deletes every r.lastNotes entry whose pod UID is absent
// from currentPodUIDs -- the same "recompute from a full listing, then drop
// what no longer applies" shape refreshPodsInTier already uses for
// r.podMemberships, reused here so a pod's note-tracking entry cannot
// outlive the pod itself in the informer cache. recordPodNotice's own
// delete-on-empty-notes path is not enough by itself: it only runs for a
// pod UID Reconcile is actually invoked with, so a pod deleted while still
// carrying a Note (the common shape -- an annotation misconfigured for the
// pod's whole lifetime) would otherwise leave its entry behind forever,
// growing r.lastNotes without bound on a node with high pod churn.
func (r *Reconciler) pruneNoteState(currentPodUIDs map[types.UID]struct{}) {
	for uid := range r.lastNotes {
		if _, stillPresent := currentPodUIDs[uid]; !stillPresent {
			delete(r.lastNotes, uid)
		}
	}
}
