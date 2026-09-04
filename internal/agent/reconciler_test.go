package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/apply"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// fakeApplier implements Applier, recording call counts instead of
// touching a filesystem or emitting Events/metrics. It exists for
// TestVC3ReconcileIsIdempotent, where the assertion under test is purely
// "did Reconcile decide to call Apply/Revert at all" — the real
// apply.Applier is exercised end to end by TestVC1AnnotatedPodGetsIdle and
// TestVC2LivePodAnnotationAddAndRemove instead.
type fakeApplier struct {
	applyCalls         int
	revertCalls        int
	applyErrors        []error
	applyReportNotices []bool
	pendingRepair      bool
	forgotUIDs         []types.UID
}

func (f *fakeApplier) Apply(_ context.Context, _ *corev1.Pod, reportNotices bool) error {
	f.applyCalls++
	f.applyReportNotices = append(f.applyReportNotices, reportNotices)
	if len(f.applyErrors) >= f.applyCalls {
		return f.applyErrors[f.applyCalls-1]
	}
	return nil
}

func (f *fakeApplier) Revert(context.Context, *corev1.Pod, apply.Snapshot) error {
	f.revertCalls++
	f.pendingRepair = false
	return nil
}

func (f *fakeApplier) NeedsWeightRepair(*corev1.Pod, apply.Snapshot) bool {
	return f.pendingRepair
}

func (f *fakeApplier) ForgetPod(uid types.UID) {
	f.forgotUIDs = append(f.forgotUIDs, uid)
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
	dir, err := cgroup.PodCgroupPath(root, cgroup.DefaultKubepodsName, driver, qosClass, uid)
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

func TestReconcileRetriesWhenRunningPodCgroupIsMissing(t *testing.T) {
	root := t.TempDir()
	pod := testPod("aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	pod.Status.Phase = corev1.PodRunning
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	fakeApply := &fakeApplier{}
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), fakeApply, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(prometheus.NewRegistry()), "node-a",
	)

	err := reconciler.Reconcile(context.Background(), pod.Namespace+"/"+pod.Name, false)
	if !errors.Is(err, cgroup.ErrCgroupGone) {
		t.Fatalf("Reconcile error = %v, want ErrCgroupGone retry for a Running pod", err)
	}
	if fakeApply.applyCalls != 0 || fakeApply.revertCalls != 0 {
		t.Fatalf("applier calls = apply:%d revert:%d, want none without a cgroup", fakeApply.applyCalls, fakeApply.revertCalls)
	}
}

func TestReconcileSkipsMirrorPodWithoutResolvedStaticUID(t *testing.T) {
	root := t.TempDir()
	pod := testPod("cccccccc-1111-2222-3333-dddddddddddd", "500m", map[string]string{
		corev1.MirrorPodAnnotationKey: "static-pod-config-hash",
		annotations.TierKey:           annotations.TierValueIdle,
	})
	pod.Status.Phase = corev1.PodRunning
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	fakeApply := &fakeApplier{}
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), fakeApply, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(prometheus.NewRegistry()), "node-a",
	)

	if err := reconciler.Reconcile(context.Background(), pod.Namespace+"/"+pod.Name, false); err != nil {
		t.Fatalf("Reconcile mirror pod error = %v, want nil because its API UID cannot identify the static-pod cgroup", err)
	}
	if fakeApply.applyCalls != 0 || fakeApply.revertCalls != 0 {
		t.Fatalf("applier calls = apply:%d revert:%d, want none for a mirror pod", fakeApply.applyCalls, fakeApply.revertCalls)
	}
}

func TestPodsInTierRefreshSkipsMirrorPodWithoutResolvedStaticUID(t *testing.T) {
	root := t.TempDir()
	pod := testPod("dddddddd-1111-2222-3333-eeeeeeeeeeee", "500m", map[string]string{
		corev1.MirrorPodAnnotationKey: "static-pod-config-hash",
	})
	pod.Status.Phase = corev1.PodRunning
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	registry := prometheus.NewRegistry()
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), &fakeApplier{}, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(registry), "node-a",
	)

	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier mirror pod error = %v, want nil because mirror pods cannot be sampled by API UID", err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if got := podsInTierSamples(t, families); len(got) != 0 {
		t.Fatalf("cpu_pods_in_tier samples = %v, want none for a mirror pod", got)
	}
}

func TestReconcileIgnoresMissingCgroupBeforePodIsRunning(t *testing.T) {
	root := t.TempDir()
	pod := testPod("bbbbbbbb-1111-2222-3333-cccccccccccc", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	pod.Status.Phase = corev1.PodPending
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	fakeApply := &fakeApplier{}
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), fakeApply, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(prometheus.NewRegistry()), "node-a",
	)

	if err := reconciler.Reconcile(context.Background(), pod.Namespace+"/"+pod.Name, false); err != nil {
		t.Fatalf("Reconcile pending pod error = %v, want nil while kubelet has not created its cgroup", err)
	}
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
		informer, err := NewInformer(client, "node-a", time.Hour)
		if err != nil {
			t.Fatalf("NewInformer() error = %v, want nil", err)
		}
		recorder, events := newTestObservers("node-a")
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(informer.Lister(), applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

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
		informer, err := NewInformer(client, "node-a", time.Hour)
		if err != nil {
			t.Fatalf("NewInformer() error = %v, want nil", err)
		}
		recorder, events := newTestObservers("node-a")
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(informer.Lister(), applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

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
// Info-level log lines, and leave cpu_resync_drift_total at 0 -- even when
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
		reconciler := NewReconciler(lister, applierFake, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

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
			if family.GetName() != "cpu_resync_drift_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				if got := metric.GetCounter().GetValue(); got != 0 {
					t.Errorf("cpu_resync_drift_total = %v, want 0", got)
				}
			}
		}
	})
}

func TestReconcileRetriesOwnedInterruptedWeightRestore(t *testing.T) {
	root := t.TempDir()
	const uid = "33333333-4444-4444-4444-444444444444"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "100", "max 100000", "0")

	pod := testPod(uid, "", nil)
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"),
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}

	applierFake := &fakeApplier{pendingRepair: true}
	metrics := observe.NewMetrics(prometheus.NewRegistry())
	reconciler := NewReconciler(corelisters.NewPodLister(indexer), applierFake, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		t.Fatalf("MetaNamespaceKeyFunc: %v", err)
	}

	if err := reconciler.Reconcile(context.Background(), key, true); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if applierFake.revertCalls != 1 || applierFake.applyCalls != 0 {
		t.Fatalf("applier calls = {apply: %d, revert: %d}, want one Revert recovery", applierFake.applyCalls, applierFake.revertCalls)
	}
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
		// seeded cgroup path) with no predicted CPU quota, so
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
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

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
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

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

func TestReconcilerReportsActualMissingQuotaOnce(t *testing.T) {
	root := t.TempDir()
	const uid = "45454545-1111-2222-3333-444444444444"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "1", "max 100000", "0")
	pod := testPod(uid, "500m", map[string]string{annotations.BurstKey: ""})
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	lister := corelisters.NewPodLister(indexer)
	fakeRecorder := record.NewFakeRecorder(10)
	recorder := observe.NewRecorder(prometheus.NewRegistry(), fakeRecorder, "node-a")
	applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, observe.NewEventRecorder(fakeRecorder))
	reconciler := NewReconciler(lister, applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, observe.NewMetrics(prometheus.NewRegistry()), "node-a")
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		t.Fatalf("MetaNamespaceKeyFunc: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := reconciler.Reconcile(context.Background(), key, i > 0); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", i+1, err)
		}
	}
	select {
	case event := <-fakeRecorder.Events:
		if !strings.Contains(event, string(observe.ReasonTierInactive)) {
			t.Fatalf("event = %q, want TierInactive", event)
		}
	default:
		t.Fatal("no TierInactive Event for burst request whose cgroup has no quota")
	}
	select {
	case event := <-fakeRecorder.Events:
		t.Fatalf("unexpected duplicate Event on unchanged resync: %q", event)
	default:
	}
}

// podsInTierSamples extracts cpu_pods_in_tier's samples from a Gather()
// snapshot, keyed by "namespace|qos_class|tier" (node is omitted: every
// sample in these tests shares the same node label).
func podsInTierSamples(t *testing.T, families []*dto.MetricFamily) map[string]float64 {
	t.Helper()
	samples := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "cpu_pods_in_tier" {
			continue
		}
		for _, metric := range family.GetMetric() {
			var namespace, qosClass, tierLabel string
			for _, label := range metric.GetLabel() {
				switch label.GetName() {
				case "namespace":
					namespace = label.GetValue()
				case "qos_class":
					qosClass = label.GetValue()
				case "tier":
					tierLabel = label.GetValue()
				}
			}
			samples[namespace+"|"+qosClass+"|"+tierLabel] = metric.GetGauge().GetValue()
		}
	}
	return samples
}

// TestPodsInTierReflectsFullNodeState covers the fix for cpu_pods_in_tier
// (previously registered but never written in production): one full scan
// seeds every pod, then a real delete key removes only that pod's confirmed
// membership without disturbing the others.
func TestPodsInTierReflectsFullNodeState(t *testing.T) {
	root := t.TempDir()

	idlePod := testPod("55555555-1111-1111-1111-111111111111", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	idlePod.Name = "idle-pod"

	burstPod := testPod("55555555-2222-2222-2222-222222222222", "500m", map[string]string{
		annotations.BurstKey: "",
	})
	burstPod.Name = "burst-pod"

	// Requests burst but declares no CPU limit: BurstActive stays false
	// (AC-4's TierInactive case), so this pod must not count toward the
	// burst tier even though it carries the annotation.
	inactiveBurstPod := testPod("55555555-3333-3333-3333-333333333333", "", map[string]string{
		annotations.BurstKey: "",
	})
	inactiveBurstPod.Name = "inactive-burst-pod"

	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(idlePod.UID),
		"1", "1", "50000 100000", "0")
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(burstPod.UID),
		"0", "1", "50000 100000", "50000")
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBestEffort, string(inactiveBurstPod.UID),
		"0", "1", "max 100000", "0")

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pod := range []*corev1.Pod{idlePod, burstPod, inactiveBurstPod} {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}
	lister := corelisters.NewPodLister(indexer)

	applierFake := &fakeApplier{}
	registry := prometheus.NewRegistry()
	metrics := observe.NewMetrics(registry)
	reconciler := NewReconciler(lister, applierFake, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

	if err := reconciler.Reconcile(context.Background(), "prod/does-not-exist", false); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	got := podsInTierSamples(t, families)
	want := map[string]float64{"prod|Burstable|idle": 1, "prod|Burstable|burst": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier samples = %v, want %v", got, want)
	}

	// Removing a pod from the cache and delivering its informer delete key
	// must shrink the gauge back down.
	if err := indexer.Delete(idlePod); err != nil {
		t.Fatalf("indexer.Delete: %v", err)
	}
	if err := reconciler.Reconcile(context.Background(), "prod/idle-pod", false); err != nil {
		t.Fatalf("Reconcile() second pass error = %v", err)
	}

	families, err = registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	got = podsInTierSamples(t, families)
	want = map[string]float64{"prod|Burstable|burst": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier samples after delete = %v, want %v", got, want)
	}
}

func TestPodsInTierUsesActualCgroupStateNotAnnotations(t *testing.T) {
	root := t.TempDir()
	requestedOnly := testPod("57575757-1111-1111-1111-111111111111", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	requestedOnly.Name = "requested-only"
	requestedOnly.Namespace = "requested"
	actualOnly := testPod("57575757-2222-2222-2222-222222222222", "500m", nil)
	actualOnly.Name = "actual-only"
	actualOnly.Namespace = "actual"

	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(requestedOnly.UID),
		"0", "1", "50000 100000", "0")
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(actualOnly.UID),
		"1", "1", "50000 100000", "0")
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pod := range []*corev1.Pod{requestedOnly, actualOnly} {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}
	registry := prometheus.NewRegistry()
	metrics := observe.NewMetrics(registry)
	reconciler := NewReconciler(corelisters.NewPodLister(indexer), &fakeApplier{}, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier() error = %v", err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]float64{"actual|Burstable|idle": 1}
	if got := podsInTierSamples(t, families); !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier = %v, want actual cgroup state %v", got, want)
	}
}

func TestPodsInTierPreservesLastKnownStateOnTransientReadFailure(t *testing.T) {
	root := t.TempDir()
	const uid = "57575757-3333-3333-3333-333333333333"
	pod := testPod(uid, "500m", map[string]string{annotations.TierKey: annotations.TierValueIdle})
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"1", "1", "50000 100000", "0")
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	registry := prometheus.NewRegistry()
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), &fakeApplier{}, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(registry), "node-a",
	)
	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier() error = %v", err)
	}

	weightPath := filepath.Join(dir, apply.KnobCPUWeight)
	if err := os.Remove(weightPath); err != nil {
		t.Fatalf("remove cpu.weight: %v", err)
	}
	if err := os.Mkdir(weightPath, 0o755); err != nil {
		t.Fatalf("replace cpu.weight with unreadable shape: %v", err)
	}
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		t.Fatalf("MetaNamespaceKeyFunc: %v", err)
	}
	if err := reconciler.Reconcile(context.Background(), key, false); err == nil {
		t.Fatal("Reconcile() error = nil, want transient snapshot failure")
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]float64{"prod|Burstable|idle": 1}
	if got := podsInTierSamples(t, families); !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier after transient read failure = %v, want last confirmed state %v", got, want)
	}
}

func TestPodsInTierFullRefreshPreservesLastKnownStateOnTransientReadFailure(t *testing.T) {
	root := t.TempDir()
	const uid = "57575757-4444-4444-4444-444444444444"
	pod := testPod(uid, "500m", map[string]string{annotations.TierKey: annotations.TierValueIdle})
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"1", "1", "50000 100000", "0")
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	registry := prometheus.NewRegistry()
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), &fakeApplier{}, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(registry), "node-a",
	)
	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier() initial error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, apply.KnobCPUWeight), []byte("not-a-weight"), 0o644); err != nil {
		t.Fatalf("corrupt cpu.weight fixture: %v", err)
	}
	if err := reconciler.refreshPodsInTier(); err == nil {
		t.Fatal("refreshPodsInTier() error = nil, want transient snapshot failure")
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]float64{"prod|Burstable|idle": 1}
	if got := podsInTierSamples(t, families); !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier after failed full refresh = %v, want last confirmed state %v", got, want)
	}
}

func TestReconcileForgetsApplierStateWhenPodIsDeleted(t *testing.T) {
	root := t.TempDir()
	const uid = "57575757-5555-5555-5555-555555555555"
	pod := testPod(uid, "500m", nil)
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "20", "50000 100000", "0")
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	applier := &fakeApplier{}
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), applier, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(prometheus.NewRegistry()), "node-a",
	)
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		t.Fatalf("MetaNamespaceKeyFunc: %v", err)
	}
	if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	if err := indexer.Delete(pod); err != nil {
		t.Fatalf("indexer.Delete: %v", err)
	}
	if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
		t.Fatalf("delete Reconcile() error = %v", err)
	}

	want := []types.UID{types.UID(uid)}
	if !reflect.DeepEqual(applier.forgotUIDs, want) {
		t.Fatalf("forgot UIDs = %v, want %v", applier.forgotUIDs, want)
	}
}

// TestReconcileLogsQoSStatusMismatch covers the fix wiring up
// qos.VerifyAgainstStatus (previously computed nowhere in production,
// resolution 14): a pod whose status.qosClass disagrees with the
// spec-computed class must log exactly one Warn line naming the
// disagreement, and the disagreement alone -- with no tier requested and an
// already-converged cgroup -- must never reach the Applier.
func TestReconcileLogsQoSStatusMismatch(t *testing.T) {
	t.Run("mismatch_logs_warn_once_and_touches_no_applier_call", func(t *testing.T) {
		root := t.TempDir()
		const uid = "66666666-1111-1111-1111-111111111111"
		// Already reverted / no tier active: this pod requests no tier, so
		// Reconcile's own plan is empty and only the QoS check produces
		// any log output.
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "100000 100000", "0")

		pod := testPod(uid, "500m", nil)
		// testPod's single container has a CPU limit but no CPU request,
		// so qos.ClassOf(pod.Spec) computes Burstable; a status of
		// Guaranteed disagrees with it.
		pod.Status.QOSClass = corev1.PodQOSGuaranteed

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		applierFake := &fakeApplier{}
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applierFake, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

		var logBuf bytes.Buffer
		reconciler.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		out := logBuf.String()
		if got := strings.Count(out, "level=WARN"); got != 1 {
			t.Fatalf("log output = %q, want exactly one WARN line for the QoS mismatch, got %d", out, got)
		}
		if !strings.Contains(out, "computed class") || !strings.Contains(out, "status.qosClass") || !strings.Contains(out, "Burstable") || !strings.Contains(out, "Guaranteed") {
			t.Fatalf("log output = %q, want it to name qos.VerifyAgainstStatus's mismatch (computed class Burstable vs status.qosClass Guaranteed)", out)
		}
		if applierFake.applyCalls != 0 || applierFake.revertCalls != 0 {
			t.Fatalf("applier calls = {apply: %d, revert: %d}, want zero: a QoS status mismatch alone must never trigger a cgroup write", applierFake.applyCalls, applierFake.revertCalls)
		}
	})

	t.Run("empty_status_is_not_a_mismatch", func(t *testing.T) {
		root := t.TempDir()
		const uid = "66666666-2222-2222-2222-222222222222"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "100000 100000", "0")

		// A freshly created pod has not had status.qosClass populated yet
		// -- testPod's fixture leaves Status zero-valued, matching that.
		pod := testPod(uid, "500m", nil)

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		applierFake := &fakeApplier{}
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applierFake, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

		var logBuf bytes.Buffer
		reconciler.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		if logBuf.Len() != 0 {
			t.Fatalf("log output = %q, want empty: an empty status.qosClass is not a mismatch (a freshly created pod has not had status populated yet)", logBuf.String())
		}
	})
}

// TestPodsInTierSharedLabelCountDecrements covers the "no stale value"
// half of the Reset-window fix on refreshPodsInTier: two pods sharing the
// same namespace/qos_class/tier series must collapse that series' value
// from 2 to 1, not merely leave it at a now-stale 2, when one of them
// leaves the cache. Since the fix replaced Reset-then-Set with
// unconditional-Set-then-delete-only-stale-keys, this also guards against
// a regression that special-cased "still present" keys into a no-op
// instead of always re-Setting them.
func TestPodsInTierSharedLabelCountDecrements(t *testing.T) {
	root := t.TempDir()
	podA := testPod("88888888-1111-1111-1111-111111111111", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	podA.Name = "idle-a"

	podB := testPod("88888888-2222-2222-2222-222222222222", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	podB.Name = "idle-b"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(podA.UID),
		"1", "1", "50000 100000", "0")
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(podB.UID),
		"1", "1", "50000 100000", "0")

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pod := range []*corev1.Pod{podA, podB} {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}
	lister := corelisters.NewPodLister(indexer)

	registry := prometheus.NewRegistry()
	metrics := observe.NewMetrics(registry)
	reconciler := NewReconciler(lister, &fakeApplier{}, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier() first pass error = %v", err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	got := podsInTierSamples(t, families)
	want := map[string]float64{"prod|Burstable|idle": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier samples = %v, want %v", got, want)
	}

	if err := indexer.Delete(podB); err != nil {
		t.Fatalf("indexer.Delete: %v", err)
	}
	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier() second pass error = %v", err)
	}
	families, err = registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	got = podsInTierSamples(t, families)
	want = map[string]float64{"prod|Burstable|idle": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu_pods_in_tier samples after one pod left = %v, want %v -- a departed pod must shrink the shared-label gauge, not leave a stale count", got, want)
	}
}

// TestPodsInTierNoObservableWindowUnderConcurrentScrape proves the fix
// documented on refreshPodsInTier's own Intent comment: a scrape landing
// concurrently with a refresh pass must never observe a series that stays
// current across that pass as zero or missing -- the "metric silently
// lies" symptom (a dashboard's pod count dipping to zero and bouncing
// back) this change exists to remove. persistentPod is never removed from
// the cache; flappingPod is added and deleted on alternating passes, so
// every refreshPodsInTier call deletes or (re)creates the flapping series
// while a concurrent goroutine polls Gather() in a tight loop and asserts
// the persistent series is always exactly 1 -- with the fix, that series
// is only ever reached by Set (never Delete), so this cannot flake: any
// observed value other than 1 is the Reset-window bug, not scheduling
// noise. Run with -race: it also exercises metrics.PodsInTier itself
// (Set/Delete from the refresh goroutine, Gather from this one) under the
// race detector, the concurrency the fixed code must tolerate.
func TestPodsInTierNoObservableWindowUnderConcurrentScrape(t *testing.T) {
	root := t.TempDir()
	persistentPod := testPod("99999999-1111-1111-1111-111111111111", "500m", map[string]string{
		annotations.TierKey: annotations.TierValueIdle,
	})
	persistentPod.Name = "persistent"
	persistentPod.Namespace = "persistent-ns"

	flappingPod := testPod("99999999-2222-2222-2222-222222222222", "500m", map[string]string{
		annotations.BurstKey: "",
	})
	flappingPod.Name = "flapping"
	flappingPod.Namespace = "flapping-ns"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(persistentPod.UID),
		"1", "1", "50000 100000", "0")
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, string(flappingPod.UID),
		"0", "1", "50000 100000", "50000")

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(persistentPod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	lister := corelisters.NewPodLister(indexer)

	registry := prometheus.NewRegistry()
	metrics := observe.NewMetrics(registry)
	reconciler := NewReconciler(lister, &fakeApplier{}, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

	// Intent: establish the persistent series before the race starts, so
	// the poll loop below only ever has to tell "still 1" apart from
	// "briefly wiped", never from "not created yet".
	if err := reconciler.refreshPodsInTier(); err != nil {
		t.Fatalf("refreshPodsInTier() initial pass error = %v", err)
	}

	const iterations = 3000
	done := make(chan struct{})
	errs := make(chan error, 1)

	// Intent: this goroutine is the only writer to both indexer and the
	// reconciler's membership maps for the rest of the test, matching
	// production's single-goroutine workqueue loop (informer.go) -- the
	// invariant that lets refreshPodsInTier skip locking that field.
	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			var mutateErr error
			if i%2 == 0 {
				mutateErr = indexer.Add(flappingPod)
			} else {
				mutateErr = indexer.Delete(flappingPod)
			}
			if mutateErr != nil {
				select {
				case errs <- fmt.Errorf("mutate flapping pod (iteration %d): %w", i, mutateErr):
				default:
				}
				return
			}
			if err := reconciler.refreshPodsInTier(); err != nil {
				select {
				case errs <- fmt.Errorf("refreshPodsInTier() (iteration %d): %w", i, err):
				default:
				}
				return
			}
		}
	}()

	const persistentKey = "persistent-ns|Burstable|idle"
pollLoop:
	for {
		select {
		case <-done:
			break pollLoop
		default:
		}
		families, err := registry.Gather()
		if err != nil {
			t.Fatalf("Gather() error = %v", err)
		}
		samples := podsInTierSamples(t, families)
		if got, present := samples[persistentKey]; !present || got != 1 {
			t.Fatalf("observed %s = %v (present=%v) during a concurrent refresh pass, want always 1 -- a series that stayed current across the pass must never be visible as reset", persistentKey, got, present)
		}
	}

	select {
	case err := <-errs:
		t.Fatalf("background refresh loop: %v", err)
	default:
	}
}

// tierApplyReasonTotal sums cpu_tier_apply_total's counter value across
// every series carrying label reason=wantReason, regardless of its other
// labels (node/namespace/qos_class/result do not vary within one test's
// single pod).
func tierApplyReasonTotal(t *testing.T, families []*dto.MetricFamily, wantReason string) float64 {
	t.Helper()
	var total float64
	for _, family := range families {
		if family.GetName() != "cpu_tier_apply_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "reason" && label.GetValue() == wantReason {
					total += metric.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// drainEvents drains every currently-buffered message off events without
// blocking, returning how many there were. It exists so a test can assert
// an exact count of Events fired across several Reconcile passes instead of
// only "at least one", which is what the deployed defect this file's
// TestPodNoteDeduplication guards against actually got wrong: the Event
// fired, correctly, on every single pass rather than only when something
// about the pod changed.
func drainEvents(events <-chan string) int {
	count := 0
	for {
		select {
		case <-events:
			count++
		default:
			return count
		}
	}
}

// TestPodNoteDeduplication covers the fix for the defect a live-cluster
// deployment surfaced: a pod carrying a tier.Note (AC-4's burst-without-
// limits.cpu case here) that Reconcile's own plan has nothing to write for
// used to route through Applier.Apply -- and so re-fire its Event and
// increment cpu_tier_apply_total -- on every single reconcile pass,
// including every ~60s resync, even though nothing about the pod ever
// changed (measured on a stand as cpu_tier_apply_total{reason=
// "limits_cpu_missing",result="inactive"} climbing 7->8 in one resync
// interval with a growing TierInactive Event count on kubectl get events).
// The fix tracks each pod's last-reported Note set in the Reconciler
// (Reconciler.lastNotes) and only lets a pass reach Apply for a Note alone
// when that set has actually changed since the last pass that reported it.
func TestPodNoteDeduplication(t *testing.T) {
	// Intent: shared fixture across every subtest below -- a Burstable pod
	// with the burst annotation but no CPU limit (Requests only), whose
	// cgroup is already seeded at the fully-cleared state BuildPlan
	// converges toward for this shape. tier.Desired reports exactly one
	// Note (NoteNoCPULimit / AC-4's TierInactive) for it, and its plan is
	// empty on every pass regardless of the annotation's presence -- see
	// TestSeamNotesReachUserThroughReconciler's own fixture, which this
	// mirrors -- so every subtest below exercises the "nothing to write,
	// something to say" seam the fix targets, never a real cgroup write.
	newBurstWithoutLimitPod := func(uid string) *corev1.Pod {
		pod := testPod(uid, "", map[string]string{annotations.BurstKey: ""})
		pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		}
		return pod
	}

	t.Run("persistent_cgroup_work_does_not_repeat_unchanged_notice", func(t *testing.T) {
		root := t.TempDir()
		const uid = "77777777-0000-0000-0000-000000000000"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBestEffort, uid,
			"1", "1", "max 100000", "0")
		pod := testPod(uid, "", map[string]string{annotations.TierKey: "future-tier"})
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		applier := &fakeApplier{}
		reconciler := NewReconciler(
			corelisters.NewPodLister(indexer), applier, root,
			cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
			observe.NewMetrics(prometheus.NewRegistry()), "node-a",
		)
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}

		for i := 0; i < 2; i++ {
			if err := reconciler.Reconcile(context.Background(), key, true); err != nil {
				t.Fatalf("Reconcile() pass %d: %v", i+1, err)
			}
		}
		if got, want := applier.applyReportNotices, []bool{true, false}; !reflect.DeepEqual(got, want) {
			t.Fatalf("Apply reportNotices flags = %v, want %v", got, want)
		}
	})

	t.Run("three_unchanged_passes_fire_exactly_once", func(t *testing.T) {
		root := t.TempDir()
		const uid = "77777777-1111-1111-1111-111111111111"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "max 100000", "0")

		pod := newBurstWithoutLimitPod(uid)

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		registry := prometheus.NewRegistry()
		fakeRecorder := record.NewFakeRecorder(10)
		recorder := observe.NewRecorder(registry, fakeRecorder, "node-a")
		events := observe.NewEventRecorder(fakeRecorder)
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}

		// Three passes over a pod whose annotations and cgroup state never
		// change, standing in for three ~60s resync ticks -- resync=true
		// on every pass, matching the informer's own periodic full resync
		// that surfaced this defect in production.
		for i := 0; i < 3; i++ {
			if err := reconciler.Reconcile(context.Background(), key, true); err != nil {
				t.Fatalf("Reconcile() pass %d error = %v", i+1, err)
			}
		}

		if got := drainEvents(fakeRecorder.Events); got != 1 {
			t.Fatalf("event count across 3 unchanged passes = %d, want exactly 1: an unchanged Note must not re-fire on every resync pass", got)
		}

		families, err := registry.Gather()
		if err != nil {
			t.Fatalf("Gather() error = %v", err)
		}
		if got := tierApplyReasonTotal(t, families, string(observe.TierApplyReasonLimitsCPUMissing)); got != 1 {
			t.Fatalf(`cpu_tier_apply_total{reason=%q} = %v, want 1: an unchanged Note must not increment the counter on every resync pass`, observe.TierApplyReasonLimitsCPUMissing, got)
		}
	})

	t.Run("note_disappearing_then_reappearing_fires_again", func(t *testing.T) {
		root := t.TempDir()
		const uid = "77777777-2222-2222-2222-222222222222"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "max 100000", "0")

		pod := newBurstWithoutLimitPod(uid)

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		fakeRecorder := record.NewFakeRecorder(10)
		recorder := observe.NewRecorder(prometheus.NewRegistry(), fakeRecorder, "node-a")
		events := observe.NewEventRecorder(fakeRecorder)
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}

		// Pass 1: the Note first appears -- fires.
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() pass 1 error = %v", err)
		}

		// Pass 2: the burst annotation is removed entirely (standing in for
		// the user adding limits.cpu -- both collapse the pod to "no Note,
		// nothing to write" the same way). The Note disappears, so this
		// pass must stay silent.
		fixed := pod.DeepCopy()
		fixed.Annotations = nil
		if err := indexer.Update(fixed); err != nil {
			t.Fatalf("indexer.Update (remove annotation): %v", err)
		}
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() pass 2 error = %v", err)
		}

		// Pass 3: the annotation comes back with the CPU limit still
		// missing -- the same Note reappears and must fire again, not stay
		// suppressed by pass 1's now-stale record (recordPodNotice deleted
		// that record when pass 2 observed zero notes).
		if err := indexer.Update(pod); err != nil {
			t.Fatalf("indexer.Update (restore annotation): %v", err)
		}
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() pass 3 error = %v", err)
		}

		gotEvents := make([]string, 0, 2)
	drainLoop:
		for {
			select {
			case got := <-fakeRecorder.Events:
				gotEvents = append(gotEvents, got)
			default:
				break drainLoop
			}
		}
		if len(gotEvents) != 2 {
			t.Fatalf("event count across appear/disappear/reappear = %d %v, want exactly 2 (one per appearance)", len(gotEvents), gotEvents)
		}
		for _, got := range gotEvents {
			if !strings.Contains(got, string(observe.ReasonTierInactive)) {
				t.Errorf("event = %q, want it to carry reason %q", got, observe.ReasonTierInactive)
			}
		}
	})

	t.Run("note_state_is_pruned_when_pod_leaves_cache", func(t *testing.T) {
		root := t.TempDir()
		const uid = "77777777-3333-3333-3333-333333333333"
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "1", "max 100000", "0")

		pod := newBurstWithoutLimitPod(uid)

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
		lister := corelisters.NewPodLister(indexer)

		fakeRecorder := record.NewFakeRecorder(10)
		recorder := observe.NewRecorder(prometheus.NewRegistry(), fakeRecorder, "node-a")
		events := observe.NewEventRecorder(fakeRecorder)
		applier := apply.NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)
		metrics := observe.NewMetrics(prometheus.NewRegistry())
		reconciler := NewReconciler(lister, applier, root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, metrics, "node-a")

		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			t.Fatalf("MetaNamespaceKeyFunc: %v", err)
		}

		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if got := len(reconciler.lastNotes); got != 1 {
			t.Fatalf("len(lastNotes) after first pass = %d, want 1: the pod's Note must be tracked", got)
		}

		if err := indexer.Delete(pod); err != nil {
			t.Fatalf("indexer.Delete: %v", err)
		}
		// A real informer delete event carries this same namespace/name key;
		// the reconciler uses its UID bookkeeping to drop notification and
		// metric state even though the pod has already left the cache.
		if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
			t.Fatalf("Reconcile() second pass error = %v", err)
		}

		if got := len(reconciler.lastNotes); got != 0 {
			t.Fatalf("len(lastNotes) after pod left the cache = %d, want 0: a departed pod's note-tracking entry must not leak", got)
		}
	})
}

func TestNoticeIsRetriedWhenApplyFailsBeforeReporting(t *testing.T) {
	root := t.TempDir()
	const uid = "77777777-5555-5555-5555-555555555555"
	seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
		"0", "1", "max 100000", "0")
	pod := testPod(uid, "", map[string]string{annotations.BurstKey: ""})
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"),
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	applier := &fakeApplier{applyErrors: []error{errors.New("transient snapshot failure")}}
	reconciler := NewReconciler(
		corelisters.NewPodLister(indexer), applier, root,
		cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs,
		observe.NewMetrics(prometheus.NewRegistry()), "node-a",
	)
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		t.Fatalf("MetaNamespaceKeyFunc: %v", err)
	}

	if err := reconciler.Reconcile(context.Background(), key, false); err == nil {
		t.Fatal("first Reconcile() error = nil, want transient Apply failure")
	}
	if err := reconciler.Reconcile(context.Background(), key, false); err != nil {
		t.Fatalf("second Reconcile() error = %v, want retry to succeed", err)
	}
	if applier.applyCalls != 2 {
		t.Fatalf("Apply calls = %d, want 2: failed notice delivery must remain retryable", applier.applyCalls)
	}
}
