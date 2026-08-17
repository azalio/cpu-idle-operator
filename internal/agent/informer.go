// Package agent wires this node's pod watch and CPU-tier reconciliation
// loop together: a SharedIndexInformer scoped to this node's pods feeds a
// rate-limited workqueue, and Reconciler drains it, calling the
// already-built internal/apply.Applier for every actual convergence. It
// never builds a controller-runtime Manager or contests leader election
// (resolution T-008): a DaemonSet is exactly one process per node, so
// there is no second agent to race for a cgroup file.
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// reconcileRequest is a single workqueue item: a pod's cache key
// (namespace/name, as cache.SplitMetaNamespaceKey expects), plus whether
// this particular enqueue came from the informer's periodic full resync
// replaying an unchanged pod rather than an observed Add/Update/Delete
// event. Reconciler needs the distinction solely to attribute a drift it
// catches to cpi_resync_drift_total (resolution T-011): resync is
// insurance against an unknown writer, and the metric exists to say "we
// found one" without drowning in ordinary apply/revert outcomes.
type reconcileRequest struct {
	key    string
	resync bool
}

// ReconcileFunc reconciles a single pod, identified by key, toward its
// desired CPU-tier state. resync is true when the call was triggered by
// the informer's periodic full resync rather than an observed event; see
// Reconciler.Reconcile.
type ReconcileFunc func(ctx context.Context, key string, resync bool) error

// Informer watches this node's pods — scoped server-side via
// spec.nodeName, never filtered in memory (a risk this subtask is named
// for: in-memory filtering would make every agent in the cluster pull
// every pod, defeating the field selector entirely) — and feeds every
// Add, Update, and Delete into a rate-limited workqueue for a
// ReconcileFunc to drain.
//
// Add-only handler registration is the single most likely mistake here: it
// passes a pod created with the tier annotation already set (AC-1) but
// silently ignores kubectl annotate/kubectl label on a live pod (AC-12),
// since client-go never re-delivers Add for an object already in the
// cache. Informer registers all three handlers to rule that out
// structurally, not just by convention.
type Informer struct {
	factory  informers.SharedInformerFactory
	informer cache.SharedIndexInformer
	lister   corelisters.PodLister
	queue    workqueue.TypedRateLimitingInterface[reconcileRequest]
}

// NewInformer builds an Informer scoped to nodeName's pods, resyncing
// every resyncPeriod (resolution T-011: 60s in production, insurance
// against an unknown writer, not the primary mechanism). It registers the
// Add/Update/Delete handlers and returns immediately; call Start or Run to
// actually begin watching.
func NewInformer(client kubernetes.Interface, nodeName string, resyncPeriod time.Duration) *Informer {
	// NewFilteredSharedInformerFactory is deprecated in favor of
	// NewSharedInformerFactoryWithOptions; the default namespace scope is
	// already metav1.NamespaceAll, so WithTweakListOptions alone
	// reproduces its exact behavior.
	factory := informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			// Intent: scope the watch to this node's pods on the API
			// server itself (VC4) — the surface this agent's RBAC and
			// the cluster's API-server load both depend on staying a
			// single-node slice, not the whole cluster's pods.
			opts.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		}),
	)

	podInformer := factory.Core().V1().Pods()
	queue := workqueue.NewTypedRateLimitingQueue[reconcileRequest](workqueue.DefaultTypedControllerRateLimiter[reconcileRequest]())

	inf := &Informer{
		factory:  factory,
		informer: podInformer.Informer(),
		lister:   podInformer.Lister(),
		queue:    queue,
	}

	inf.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			inf.enqueue(obj, false)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			inf.enqueue(newObj, isResync(oldObj, newObj))
		},
		DeleteFunc: func(obj interface{}) {
			inf.enqueue(obj, false)
		},
	})

	return inf
}

// isResync reports whether oldObj and newObj — an UpdateFunc handler's two
// arguments — represent the informer's periodic full resync replaying an
// unchanged pod rather than a genuine spec/annotation change: on resync,
// client-go redelivers the identical cached object as both old and new, so
// they share the same, non-empty ResourceVersion. Any type-assertion
// failure or an empty ResourceVersion is treated as "not resync" — a
// real event, never silently miscounted as insurance-only drift.
func isResync(oldObj, newObj interface{}) bool {
	oldPod, ok := oldObj.(*corev1.Pod)
	if !ok {
		return false
	}
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		return false
	}
	return oldPod.ResourceVersion != "" && oldPod.ResourceVersion == newPod.ResourceVersion
}

// enqueue converts obj to its cache key and adds it to the workqueue.
// cache.DeletionHandlingMetaNamespaceKeyFunc unwraps the
// cache.DeletedFinalStateUnknown wrapper DeleteFunc may hand back when the
// deletion was observed via a relist rather than a live watch event, so
// this one helper covers all three handlers.
func (inf *Informer) enqueue(obj interface{}, resync bool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	inf.queue.Add(reconcileRequest{key: key, resync: resync})
}

// Lister returns the pod lister backing this Informer's cache. Reconciler
// reads pods through it exclusively — never a direct API call — so a
// reconcile pass never blocks on API-server latency.
func (inf *Informer) Lister() corelisters.PodLister {
	return inf.lister
}

// Start begins watching the API server and blocks until the informer's
// cache has performed its initial sync or ctx is done, whichever comes
// first. It returns false only when ctx was done before the cache synced.
func (inf *Informer) Start(ctx context.Context) bool {
	go inf.factory.Start(ctx.Done())
	return cache.WaitForCacheSync(ctx.Done(), inf.informer.HasSynced)
}

// Run starts the informer (see Start), then blocks draining the
// workqueue — calling reconcile for every key, in the exact order the
// queue delivers them — until ctx is done. Manager.Start from
// controller-runtime would do this and leader election together; this
// type deliberately does only the informer/workqueue half (HC-4).
func (inf *Informer) Run(ctx context.Context, reconcile ReconcileFunc) error {
	if !inf.Start(ctx) {
		return fmt.Errorf("agent: informer: cache did not sync: %w", ctx.Err())
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		inf.queue.ShutDown()
	}()

	for inf.processNextItem(ctx, reconcile) {
	}
	wg.Wait()
	return nil
}

// processNextItem drains exactly one item from the workqueue and calls
// reconcile with it, requeueing with rate-limited backoff on error
// (resolution T-011's error edge case: a failed apply must not roll the
// whole loop). It reports false once the queue has been shut down and
// drained, the signal Run's loop uses to stop.
func (inf *Informer) processNextItem(ctx context.Context, reconcile ReconcileFunc) bool {
	req, shutdown := inf.queue.Get()
	if shutdown {
		return false
	}
	defer inf.queue.Done(req)

	if err := reconcile(ctx, req.key, req.resync); err != nil {
		inf.queue.AddRateLimited(req)
		return true
	}
	inf.queue.Forget(req)
	return true
}
