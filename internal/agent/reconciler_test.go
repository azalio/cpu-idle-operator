package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/azalio/cpi-idle-operator/internal/annotations"
	"github.com/azalio/cpi-idle-operator/internal/apply"
	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/observe"
	"github.com/azalio/cpi-idle-operator/internal/qos"
)

// fakeApplier implements Applier, recording call counts instead of
// touching a filesystem or emitting Events/metrics. It exists for
// TestVC3ReconcileIsIdempotent, where the assertion under test is purely
// "did Reconcile decide to call Apply/Revert at all" — the real
// apply.Applier is exercised end to end by TestVC1AnnotatedPodGetsIdle and
// TestVC2LivePodAnnotationAddAndRemove instead.
type fakeApplier struct {
	applyCalls  int
	revertCalls int
}

func (f *fakeApplier) Apply(context.Context, *corev1.Pod) error {
	f.applyCalls++
	return nil
}

func (f *fakeApplier) Revert(context.Context, *corev1.Pod, apply.Snapshot) error {
	f.revertCalls++
	return nil
}

// testPod builds a minimal single-container Burstable-shaped pod (a
// positive CPU limit, no CPU request) carrying annos, mirroring
// internal/apply's own test fixture builder (apply_test.go's testPod)
// since that helper is unexported to its package.
func testPod(uid, cpuLimit string, annos map[string]string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "web-1",
			Namespace:       "prod",
			UID:             types.UID(uid),
			ResourceVersion: "1",
			Annotations:     annos,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	if cpuLimit != "" {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpuLimit),
		}
	}
	return pod
}

// seedPodCgroup creates dir (computed the same way Applier/Reconciler
// compute it) and writes the four knob files apply.ReadSnapshot reads. It
// returns dir so a test can assert file content against it directly.
func seedPodCgroup(t *testing.T, root string, driver cgroup.Driver, qosClass cgroup.QoSClass, uid string, idle, weight, max, burst string) string {
	t.Helper()
	dir, err := cgroup.PodCgroupPath(root, driver, qosClass, uid)
	if err != nil {
		t.Fatalf("PodCgroupPath: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	files := map[string]string{
		apply.KnobCPUIdle:     idle,
		apply.KnobCPUWeight:   weight,
		apply.KnobCPUMax:      max,
		apply.KnobCPUMaxBurst: burst,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

// newTestObservers builds a real Recorder/EventRecorder pair backed by a
// fresh Prometheus registry and a client-go FakeRecorder, the same wiring
// apply.NewApplier expects from its caller in production.
func newTestObservers(node string) (*observe.Recorder, *observe.EventRecorder) {
	registry := prometheus.NewRegistry()
	fakeRecorder := record.NewFakeRecorder(100)
	recorder := observe.NewRecorder(registry, fakeRecorder, node)
	events := observe.NewEventRecorder(fakeRecorder)
	return recorder, events
}

// waitForKnobContent polls dir/name until its content equals want or
// timeout elapses. It never asserts a numeric SLA on how fast convergence
// happens (spec_default.md's AC-1 explicitly disclaims one) — only that it
// happens within a generous bound, so the test fails fast on a real
// regression instead of hanging until Go's own test timeout.
func waitForKnobContent(t *testing.T, dir, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastGot string
	var lastErr error
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case err != nil:
			lastErr = err
		case string(got) == want:
			return
		default:
			lastGot = string(got)
			lastErr = nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s content did not become %q within %s (last content = %q, last read error = %v)", name, want, timeout, lastGot, lastErr)
}

// TestVC1AnnotatedPodGetsIdle covers VC1 [AC-1]: a pod carrying the idle
// tier annotation, present in the fake clientset before the informer ever
// starts, converges to cpu.idle=1 on its pod cgroup within a generous
// wait -- driven through the real Informer -> workqueue -> Reconciler ->
// apply.Applier -> cgroup.WriteKnob stack, not a direct Reconcile call.
func TestVC1AnnotatedPodGetsIdle(t *testing.T) {
	t.Run("test_vc1_annotated_pod_gets_idle", func(t *testing.T) {
		root := t.TempDir()
		const uid = "11111111-1111-1111-1111-111111111111"
		dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "20", "100000 100000", "0")

		pod := testPod(uid, "500m", map[string]string{
			annotations.TierKey: annotations.TierValueIdle,
		})

		client := fake.NewSimpleClientset(pod)
		informer := NewInformer(client, "node-a", time.Hour)
		recorder, events := newTestObservers("node-a")
		applier := apply.NewApplier(root, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(informer.Lister(), applier, root, cgroup.DriverCgroupfs, metrics, "node-a")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			_ = informer.Run(ctx, reconciler.Reconcile)
		}()

		waitForKnobContent(t, dir, apply.KnobCPUIdle, "1", 5*time.Second)
	})
}

// TestVC2LivePodAnnotationAddAndRemove covers VC2 [AC-12]: the tier
// annotation added to an *already-existing* pod via an Update, then
// removed via another Update, must apply and then revert the tier through
// the same Informer -> Reconciler stack. An implementation that only
// registers AddFunc (the trap AC-12 exists to catch, per resolution
// T-011's review note and this subtask's own risk list) never observes
// either Update: the first assertion below times out waiting for
// cpu.idle to become "1", since the annotation Update is never delivered
// to Reconcile. That failure is fatal and stops the subtest there, so the
// later assertions never run against this mutant -- had they run, they
// would have passed trivially, since a pod nothing ever touched already
// sits at the reverted values seedPodCgroup wrote. The test's cache-sync
// wait below (informer.Start before the first Update) exists so this
// failure is deterministic: without it, the fake clientset's Update can
// race the informer's own initial List, letting the first assertion pass
// by accident instead.
func TestVC2LivePodAnnotationAddAndRemove(t *testing.T) {
	t.Run("test_vc2_live_pod_annotation_add_and_remove", func(t *testing.T) {
		root := t.TempDir()
		const uid = "22222222-2222-2222-2222-222222222222"
		dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "100000 100000", "0")

		pod := testPod(uid, "500m", nil)

		client := fake.NewSimpleClientset(pod)
		informer := NewInformer(client, "node-a", time.Hour)
		recorder, events := newTestObservers("node-a")
		applier := apply.NewApplier(root, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(informer.Lister(), applier, root, cgroup.DriverCgroupfs, metrics, "node-a")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Intent: block until the informer's cache has performed its
		// initial sync before issuing the first Update below. Without
		// this, the fake clientset's synchronous Update can land before
		// the informer's Reflector ever lists the pod, so that initial
		// List (not the Update's Watch event) is the one that observes
		// the annotation -- letting an Add-only handler registration pass
		// this test's first assertion by accident instead of failing it.
		if !informer.Start(ctx) {
			t.Fatalf("informer cache did not sync")
		}
		go func() {
			_ = informer.Run(ctx, reconciler.Reconcile)
		}()

		annotated := pod.DeepCopy()
		annotated.ResourceVersion = "2"
		annotated.Annotations = map[string]string{annotations.TierKey: annotations.TierValueIdle}
		if _, err := client.CoreV1().Pods(pod.Namespace).Update(ctx, annotated, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("Update (add annotation) error = %v", err)
		}
		waitForKnobContent(t, dir, apply.KnobCPUIdle, "1", 5*time.Second)

		removed := annotated.DeepCopy()
		removed.ResourceVersion = "3"
		removed.Annotations = nil
		if _, err := client.CoreV1().Pods(pod.Namespace).Update(ctx, removed, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("Update (remove annotation) error = %v", err)
		}
		waitForKnobContent(t, dir, apply.KnobCPUIdle, "0", 5*time.Second)

		wantWeight := strconv.FormatUint(qos.RestoreWeight(pod.Spec), 10)
		waitForKnobContent(t, dir, apply.KnobCPUWeight, wantWeight, 5*time.Second)
	})
}

// TestVC3ReconcileIsIdempotent covers VC3 [INV-6]: a second Reconcile pass
// over a pod whose cgroup already matches its desired state must produce
// zero Applier calls, zero Events (implied by zero Applier calls, since
// every Event this package can raise flows through Applier), zero
// Info-level log lines, and leave cpi_resync_drift_total at 0 -- even when
// the pass is attributed to resync.
func TestVC3ReconcileIsIdempotent(t *testing.T) {
	t.Run("test_vc3_reconcile_is_idempotent", func(t *testing.T) {
		root := t.TempDir()
		const uid = "33333333-3333-3333-3333-333333333333"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"1", "1", "100000 100000", "0")

		pod := testPod(uid, "500m", map[string]string{
			annotations.TierKey: annotations.TierValueIdle,
		})

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		applierFake := &fakeApplier{}
		registry := prometheus.NewRegistry()
		metrics := observe.NewMetrics(registry)
		reconciler := NewReconciler(lister, applierFake, root, cgroup.DriverCgroupfs, metrics, "node-a")

		var logBuf bytes.Buffer
		reconciler.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}

		// Two passes, the second one attributed to resync -- INV-6 must
		// hold for both: a converged pod produces no work regardless of
		// what triggered the pass.
		for i, resync := range []bool{false, true} {
			if err := reconciler.Reconcile(context.Background(), key, resync); err != nil {
				t.Fatalf("Reconcile() pass %d (resync=%v) error = %v", i+1, resync, err)
			}
		}

		if applierFake.applyCalls != 0 || applierFake.revertCalls != 0 {
			t.Errorf("applier calls = {apply: %d, revert: %d}, want zero", applierFake.applyCalls, applierFake.revertCalls)
		}
		if logBuf.Len() != 0 {
			t.Errorf("log output = %q, want empty: a converged pod must not log a line at Info level (INV-6)", logBuf.String())
		}

		families, err := registry.Gather()
		if err != nil {
			t.Fatalf("Gather() error = %v", err)
		}
		for _, family := range families {
			if family.GetName() != "cpi_resync_drift_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				if got := metric.GetCounter().GetValue(); got != 0 {
					t.Errorf("cpi_resync_drift_total = %v, want 0", got)
				}
			}
		}
	})
}

// TestSeamNotesReachUserThroughReconciler covers the seam this subtask's
// second defect was found in: the early return Reconcile takes when its own
// plan is empty (added for INV-6) used to run before Applier.Apply was ever
// called, and Applier.reportNotes -- the only place that turns a burst
// request with no CPU limit (AC-4) or an unrecognized tier value (AC-16)
// into an Event -- lives inside Apply. A pod shaped either way converges to
// zero cgroup writes, so the early return swallowed the Event along with
// the (correctly) skipped write, in production, not just in a unit test
// that calls Apply directly and so never exercises Reconcile's own early
// return at all.
func TestSeamNotesReachUserThroughReconciler(t *testing.T) {
	t.Run("burst_without_cpu_limit_still_fires_tier_inactive", func(t *testing.T) {
		root := t.TempDir()
		const uid = "44444444-1111-1111-1111-111111111111"
		// Already at the converged "nothing active" state: no CPU limit
		// means BurstActive stays false, so Desired's plan needs zero
		// writes regardless of this fix -- the Note must reach the user
		// anyway.
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "max 100000", "0")

		// A CPU request with no CPU limit: Burstable QoS (matching the
		// seeded cgroup path) with hasPositiveCPULimit false, so
		// BurstActive stays false and Desired's plan needs zero writes --
		// exactly the shape AC-4's Note exists for.
		pod := testPod(uid, "", map[string]string{annotations.BurstKey: ""})
		pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		}

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		fakeRecorder := record.NewFakeRecorder(10)
		recorder := observe.NewRecorder(prometheus.NewRegistry(), fakeRecorder, "node-a")
		events := observe.NewEventRecorder(fakeRecorder)
		applier := apply.NewApplier(root, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DriverCgroupfs, metrics, "node-a")

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		select {
		case got := <-fakeRecorder.Events:
			if !strings.Contains(got, string(observe.ReasonTierInactive)) {
				t.Errorf("event = %q, want it to carry reason %q", got, observe.ReasonTierInactive)
			}
		default:
			t.Fatal("Reconcile() produced no Event: a burst request with no CPU limit must still fire TierInactive (AC-4) even though it plans zero cgroup writes")
		}
	})

	t.Run("unknown_tier_value_still_fires_tier_value_unknown", func(t *testing.T) {
		root := t.TempDir()
		const uid = "44444444-2222-2222-2222-222222222222"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBestEffort, uid,
			"0", "1", "max 100000", "0")

		// tier: bogus alone (no burst annotation) requests no active tier
		// at all -- IdleRequested and BurstRequested both stay false -- so
		// this pod used to route to Revert, which never calls tier.Desired
		// and so can never see the Note, regardless of the early-return fix
		// alone.
		pod := testPod(uid, "", map[string]string{annotations.TierKey: "bogus"})

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		fakeRecorder := record.NewFakeRecorder(10)
		recorder := observe.NewRecorder(prometheus.NewRegistry(), fakeRecorder, "node-a")
		events := observe.NewEventRecorder(fakeRecorder)
		applier := apply.NewApplier(root, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DriverCgroupfs, metrics, "node-a")

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		select {
		case got := <-fakeRecorder.Events:
			if !strings.Contains(got, string(observe.ReasonTierValueUnknown)) {
				t.Errorf("event = %q, want it to carry reason %q", got, observe.ReasonTierValueUnknown)
			}
		default:
			t.Fatal("Reconcile() produced no Event: an unrecognized tier value must still fire TierValueUnknown (AC-16) even though it requests no active tier and plans zero cgroup writes")
		}
	})
}
