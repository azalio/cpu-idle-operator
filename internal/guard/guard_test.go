package guard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// The decider must need two consecutive far-side samples per transition,
// in both directions, so one noisy sample never flips the node.
func TestDeciderHysteresis(t *testing.T) {
	d := &decider{}
	high, low := 0.70, 0.60

	steps := []struct {
		util float64
		hot  bool
	}{
		{0.50, false}, // cool, below everything
		{0.75, false}, // first hot sample: not yet
		{0.55, false}, // streak broken
		{0.75, false}, // first again
		{0.80, true},  // second consecutive: hot
		{0.65, true},  // inside the band: stays hot
		{0.55, true},  // first cool sample: not yet
		{0.75, true},  // streak broken, still hot
		{0.55, true},  // first again
		{0.50, false}, // second consecutive: cool
	}
	for i, step := range steps {
		if got := d.observe(step.util, high, low); got != step.hot {
			t.Fatalf("step %d: util=%v got hot=%v, want %v", i, step.util, got, step.hot)
		}
	}
}

func TestReadUsageUsec(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cpu.stat")
	if err := os.WriteFile(p, []byte("usage_usec 123456\nuser_usec 100\nsystem_usec 200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readUsageUsec(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != 123456 {
		t.Fatalf("got %d, want 123456", got)
	}
	if err := os.WriteFile(p, []byte("nr_periods 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readUsageUsec(p); err == nil {
		t.Fatal("expected error for cpu.stat without usage_usec")
	}
}

type guardHarness struct {
	guard    *Guard
	client   *fake.Clientset
	indexer  cache.Indexer
	pod      *corev1.Pod
	dir      string
	registry *prometheus.Registry
	events   *record.FakeRecorder
	recorder *observe.Recorder
}

func newGuardHarness(t *testing.T, cpuMax string) *guardHarness {
	t.Helper()
	root := t.TempDir()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "idle-work",
			Namespace:   "jobs",
			UID:         types.UID("aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb"),
			Annotations: map[string]string{annotations.TierKey: annotations.TierValueIdle},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}, NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	dir, err := cgroup.PodCgroupPath(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, qos.ToCgroupClass(qos.ClassOf(pod.Spec)), string(pod.UID))
	if err != nil {
		t.Fatalf("PodCgroupPath: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir pod cgroup: %v", err)
	}
	for name, value := range map[string]string{
		"cpu.stat": "usage_usec 100\n",
		"cpu.idle": "1",
		"cpu.max":  cpuMax,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	client := fake.NewSimpleClientset(pod.DeepCopy())
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(pod.DeepCopy()); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	registry := prometheus.NewRegistry()
	events := record.NewFakeRecorder(20)
	recorder := observe.NewRecorder(registry, events, "node-a")
	cfg := Config{
		High:         0.7,
		Low:          0.6,
		Period:       time.Hour,
		FloorQuota:   "10000 100000",
		CgroupRoot:   root,
		KubepodsName: cgroup.DefaultKubepodsName,
		Driver:       cgroup.DriverCgroupfs,
		NodeName:     "node-a",
	}
	g := New(cfg, client, corelisters.NewPodLister(indexer), recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &guardHarness{guard: g, client: client, indexer: indexer, pod: pod, dir: dir, registry: registry, events: events, recorder: recorder}
}

func (h *guardHarness) apiPod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := h.client.CoreV1().Pods(h.pod.Namespace).Get(context.Background(), h.pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return pod
}

func (h *guardHarness) updateIndexer(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if err := h.indexer.Update(pod.DeepCopy()); err != nil {
		t.Fatalf("indexer.Update: %v", err)
	}
}

func (h *guardHarness) setInitialCPULimit(t *testing.T, value string) {
	t.Helper()
	pod := h.apiPod(t)
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(value),
	}
	updated, err := h.client.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("set initial CPU limit: %v", err)
	}
	newDir, err := cgroup.PodCgroupPath(
		h.guard.cfg.CgroupRoot, h.guard.cfg.KubepodsName, h.guard.cfg.Driver,
		qos.ToCgroupClass(qos.ClassOf(updated.Spec)), string(updated.UID),
	)
	if err != nil {
		t.Fatalf("compute cgroup after CPU limit: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		t.Fatalf("mkdir new QoS parent: %v", err)
	}
	if err := os.Rename(h.dir, newDir); err != nil {
		t.Fatalf("move fixture to its Burstable cgroup path: %v", err)
	}
	h.pod = updated.DeepCopy()
	h.dir = newDir
	h.updateIndexer(t, updated)
}

func readGuardKnob(t *testing.T, dir, knob string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(dir, knob))
	if err != nil {
		t.Fatalf("read %s: %v", knob, err)
	}
	return string(value)
}

func metricCount(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var total float64
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
		return total
	}
	return 0
}

func drainGuardEvents(events *record.FakeRecorder) []string {
	var got []string
	for {
		select {
		case event := <-events.Events:
			got = append(got, event)
		default:
			return got
		}
	}
}

func TestIdlePodsUsageRequiresLiveIdleState(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	if err := os.WriteFile(filepath.Join(h.dir, "cpu.idle"), []byte("0"), 0o644); err != nil {
		t.Fatalf("set cpu.idle inactive: %v", err)
	}

	candidates, usage, err := h.guard.idlePodsUsage([]*corev1.Pod{h.pod})
	if err != nil {
		t.Fatalf("idlePodsUsage() error = %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want none while live cpu.idle is inactive", candidates)
	}
	if len(usage) != 0 {
		t.Fatalf("usage = %v, want none: a merely annotated pod must count as non-idle load", usage)
	}
}

func TestIdlePodsUsageReportsUnreadableCPUStat(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	if err := os.Remove(filepath.Join(h.dir, "cpu.stat")); err != nil {
		t.Fatalf("remove cpu.stat: %v", err)
	}

	candidates, usage, err := h.guard.idlePodsUsage([]*corev1.Pod{h.pod})
	if err == nil {
		t.Fatal("idlePodsUsage() error = nil, want missing cpu.stat reported while cgroup exists")
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v, want live idle pod retained for convergence", candidates)
	}
	if len(usage) != 0 {
		t.Fatalf("usage = %v, want no fabricated sample", usage)
	}
}

func TestGuardRestoresSuppressionWhenTierAnnotationIsRemoved(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max = %q, want guard floor", got)
	}
	marked := h.apiPod(t)
	if marked.Annotations[annotations.GuardStateKey] == "" {
		t.Fatal("guard ownership marker was not persisted before suppression")
	}

	delete(marked.Annotations, annotations.TierKey)
	updated, err := h.client.CoreV1().Pods(marked.Namespace).Update(ctx, marked, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("remove tier annotation: %v", err)
	}
	h.updateIndexer(t, updated)
	if err := h.guard.converge(ctx, true, []*corev1.Pod{updated}, nil); err != nil {
		t.Fatalf("restore ineligible pod: %v", err)
	}

	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want exact restored value", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
		t.Fatalf("guard marker = %q, want removed after restore", got)
	}
	if got := metricCount(t, h.registry, "cpu_tier_apply_total"); got != 2 {
		t.Fatalf("cpu_tier_apply_total = %v, want suppression and restoration paired with metrics", got)
	}
	events := strings.Join(drainGuardEvents(h.events), "\n")
	if !strings.Contains(events, "IdleSuppressed") || !strings.Contains(events, "IdleRestored") {
		t.Fatalf("events = %q, want IdleSuppressed and IdleRestored", events)
	}
}

func TestGuardUsesLiveQuotaInsteadOfSpecPrediction(t *testing.T) {
	t.Run("unbounded cgroup remains eligible despite spec limit", func(t *testing.T) {
		h := newGuardHarness(t, "max 100000")
		h.setInitialCPULimit(t, "500m")
		if err := h.guard.converge(context.Background(), true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
			t.Fatalf("converge: %v", err)
		}
		if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
			t.Fatalf("cpu.max = %q, want guard floor because live cgroup is unbounded", got)
		}
	})

	t.Run("finite cgroup is ineligible despite missing spec limit", func(t *testing.T) {
		h := newGuardHarness(t, "50000 100000")
		if err := h.guard.converge(context.Background(), true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
			t.Fatalf("converge: %v", err)
		}
		if got := readGuardKnob(t, h.dir, "cpu.max"); got != "50000 100000" {
			t.Fatalf("cpu.max = %q, want finite live quota preserved", got)
		}
		if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
			t.Fatalf("guard marker = %q, want none for an ineligible pod", got)
		}
	})
}

func TestGuardKeepsOwnedFloorWhileNodeRemainsHot(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
			t.Fatalf("converge pass %d: %v", i, err)
		}
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max = %q, want owned guard floor", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got == "" {
		t.Fatal("guard marker was cleared while suppression remained desired")
	}
}

func TestGuardRestoresOwnedPodThatLeavesInformerCache(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if err := h.guard.converge(ctx, true, nil, nil); err != nil {
		t.Fatalf("converge after cache deletion: %v", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want restored after pod left cache", got)
	}
	if len(h.guard.owned) != 0 {
		t.Fatalf("owned entries = %d, want none after cache deletion", len(h.guard.owned))
	}
}

func TestGuardRestoresTerminatingPod(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	terminating := h.pod.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	h.updateIndexer(t, terminating)
	candidates, _, err := h.guard.idlePodsUsage([]*corev1.Pod{terminating})
	if err != nil {
		t.Fatalf("idlePodsUsage: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("terminating pod remained a suppression candidate: %+v", candidates)
	}
	if err := h.guard.converge(ctx, true, []*corev1.Pod{terminating}, candidates); err != nil {
		t.Fatalf("restore terminating pod: %v", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want terminating pod restored", got)
	}
}

func TestGuardDoesNotAttachOldStateToReplacementPod(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.client.CoreV1().Pods(h.pod.Namespace).Delete(ctx, h.pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete original pod: %v", err)
	}
	replacement := h.pod.DeepCopy()
	replacement.UID = types.UID("cccccccc-1111-2222-3333-dddddddddddd")
	if _, err := h.client.CoreV1().Pods(replacement.Namespace).Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create replacement pod: %v", err)
	}

	err := h.guard.suppressPod(ctx, h.pod)
	if !errors.Is(err, cgroup.ErrCgroupGone) {
		t.Fatalf("suppress stale pod error = %v, want ErrCgroupGone", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("old cpu.max = %q, want unchanged without durable ownership", got)
	}
	current, err := h.client.CoreV1().Pods(replacement.Namespace).Get(ctx, replacement.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get replacement pod: %v", err)
	}
	if current.Annotations[annotations.GuardStateKey] != "" {
		t.Fatalf("replacement received stale guard marker %q", current.Annotations[annotations.GuardStateKey])
	}
}

func TestGuardRecoverRestoresExactValueAcrossRestart(t *testing.T) {
	h := newGuardHarness(t, "max 250000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max = %q, want guard floor", got)
	}

	marked := h.apiPod(t)
	h.updateIndexer(t, marked)
	restarted := New(h.guard.cfg, h.client, corelisters.NewPodLister(h.indexer), h.recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := restarted.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 250000" {
		t.Fatalf("cpu.max = %q, want exact pre-guard value", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
		t.Fatalf("guard marker = %q, want removed", got)
	}
}

func TestGuardRecoveryDoesNotClearMarkerChangedAfterCacheRead(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()

	cached := h.pod.DeepCopy()
	cached.Annotations[annotations.GuardStateKey] = "not-json"
	h.updateIndexer(t, cached)

	current := h.apiPod(t)
	newMarker := `{"version":1,"knob":"cpu.max","restore":"max 200000","suppressed":"10000 100000"}`
	current.Annotations[annotations.GuardStateKey] = newMarker
	if _, err := h.client.CoreV1().Pods(current.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("replace marker in API: %v", err)
	}

	if err := h.guard.Recover(ctx); err == nil {
		t.Fatal("Recover error = nil, want stale marker cleanup rejected")
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != newMarker {
		t.Fatalf("guard marker = %q, want newer API value %q preserved", got, newMarker)
	}
}

func TestGuardRestoreForgetsLocalOwnershipWhenMarkerChanged(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()

	if err := h.guard.suppressPod(ctx, h.pod); err != nil {
		t.Fatalf("suppressPod: %v", err)
	}
	state := h.guard.owned[string(h.pod.UID)].state

	current := h.apiPod(t)
	newMarker := `{"version":1,"knob":"cpu.max","restore":"max 200000","suppressed":"10000 100000"}`
	current.Annotations[annotations.GuardStateKey] = newMarker
	updated, err := h.client.CoreV1().Pods(current.Namespace).Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("replace marker in API: %v", err)
	}

	changed, err := h.guard.restorePod(ctx, h.pod, state)
	if !changed {
		t.Fatal("restorePod changed = false, want restored cgroup value")
	}
	if !errors.Is(err, errOwnershipMarkerChanged) {
		t.Fatalf("restorePod error = %v, want ownership-marker conflict", err)
	}
	if _, ok := h.guard.owned[string(h.pod.UID)]; ok {
		t.Fatal("obsolete local ownership retained after marker conflict")
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want trusted local restore", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != newMarker {
		t.Fatalf("guard marker = %q, want replacement %q preserved", got, newMarker)
	}

	h.updateIndexer(t, updated)
	gotState, marked, stateErr := h.guard.stateForPod(updated)
	if stateErr != nil {
		t.Fatalf("stateForPod: %v", stateErr)
	}
	if !marked || gotState.Restore != "max 200000" {
		t.Fatalf("stateForPod = (%+v, %v), want replacement marker", gotState, marked)
	}
}

func TestGuardSuppressionDoesNotOverwriteMarkerMissingFromStaleCache(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()

	current := h.apiPod(t)
	newMarker := `{"version":1,"knob":"cpu.max","restore":"max 200000","suppressed":"10000 100000"}`
	current.Annotations[annotations.GuardStateKey] = newMarker
	if _, err := h.client.CoreV1().Pods(current.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("set marker in API: %v", err)
	}

	err := h.guard.suppressPod(ctx, h.pod)
	if !errors.Is(err, errOwnershipMarkerChanged) {
		t.Fatalf("suppressPod error = %v, want ownership-marker conflict", err)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != newMarker {
		t.Fatalf("guard marker = %q, want concurrent API value %q preserved", got, newMarker)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want no write without acquired ownership", got)
	}
}

func TestGuardRecoveryRefusesForgedUnboundedRestoreOnLimitedPod(t *testing.T) {
	h := newGuardHarness(t, "10000 100000")
	h.setInitialCPULimit(t, "500m")
	ctx := context.Background()

	forged := h.apiPod(t)
	forged.Annotations[annotations.GuardStateKey] = `{"version":1,"knob":"cpu.max","restore":"max 100000","suppressed":"10000 100000"}`
	updated, err := h.client.CoreV1().Pods(forged.Namespace).Update(ctx, forged, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("add forged guard marker: %v", err)
	}
	h.updateIndexer(t, updated)

	restarted := New(h.guard.cfg, h.client, corelisters.NewPodLister(h.indexer), h.recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := restarted.Recover(ctx); err == nil {
		t.Fatal("Recover error = nil, want forged marker rejected")
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max = %q, want finite value preserved against forged marker", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got == "" {
		t.Fatal("guard marker was removed, want ambiguous ownership evidence retained")
	}
	if got := metricCount(t, h.registry, "cpu_tier_apply_total"); got != 0 {
		t.Fatalf("cpu_tier_apply_total = %v, want no claimed cgroup change", got)
	}
}

func TestGuardDoesNotOverwriteNewKubeletQuotaWhenEligibilityChanges(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	marked := h.apiPod(t)
	marked.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"),
	}
	updated, err := h.client.CoreV1().Pods(marked.Namespace).Update(ctx, marked, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("add CPU limit: %v", err)
	}
	h.updateIndexer(t, updated)
	if err := os.WriteFile(filepath.Join(h.dir, "cpu.max"), []byte("50000 100000"), 0o644); err != nil {
		t.Fatalf("simulate kubelet quota update: %v", err)
	}
	if err := h.guard.converge(ctx, true, []*corev1.Pod{updated}, nil); err != nil {
		t.Fatalf("drop ineligible guard ownership: %v", err)
	}

	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "50000 100000" {
		t.Fatalf("cpu.max = %q, want kubelet's newer quota preserved", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
		t.Fatalf("guard marker = %q, want removed", got)
	}
}

func TestGuardRestoresOwnedFloorWhenQuotaEnforcementIsDisabled(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	h.setInitialCPULimit(t, "100m")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	// The Pod asks for a limit, but a kubelet running with CPU quota
	// enforcement disabled leaves cpu.max untouched. The bytes still carry
	// the exact suppression transition owned by the marker, so cleanup must
	// restore them instead of abandoning a permanent throttle.
	if err := h.guard.converge(ctx, false, []*corev1.Pod{h.pod}, nil); err != nil {
		t.Fatalf("cool guard: %v", err)
	}

	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want exact pre-guard value restored", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
		t.Fatalf("guard marker = %q, want ownership relinquished", got)
	}
}

func TestGuardRecoveryUsesOwnedTransitionDespiteStaleInformer(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	// Change only the API object. The lister remains deliberately stale and
	// kubelet never changes cpu.max, as happens when CPU quota enforcement is
	// disabled. Recovery must be driven by the owned cgroup transition, not
	// by either copy of the Pod spec.
	latest := h.apiPod(t)
	latest.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"),
	}
	if _, err := h.client.CoreV1().Pods(latest.Namespace).Update(ctx, latest, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("add CPU limit in API: %v", err)
	}
	if err := h.guard.converge(ctx, false, []*corev1.Pod{h.pod}, nil); err != nil {
		t.Fatalf("cool guard with stale lister: %v", err)
	}

	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want exact pre-guard value restored", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
		t.Fatalf("guard marker = %q, want ownership relinquished", got)
	}
}

func TestGuardRunDoesNotChangeOwnedStateOnShutdown(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	if err := h.guard.converge(context.Background(), true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.guard.Run(ctx); err != nil {
		t.Fatalf("Run after cancellation: %v", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max after Run returned = %q, want shutdown to preserve state", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got == "" {
		t.Fatal("guard marker was removed during shutdown")
	}
}

func TestGuardDoesNotWriteAfterCancellation(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	ctx := context.Background()
	if err := h.guard.converge(ctx, true, []*corev1.Pod{h.pod}, []*corev1.Pod{h.pod}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	state := h.guard.owned[string(h.pod.UID)].state
	if _, err := h.guard.restorePod(canceled, h.pod, state); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore after cancellation error = %v, want context.Canceled", err)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max after cancelled restore = %q, want unchanged", got)
	}
}

func TestGuardReportsTickFailureAndRecovery(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	var reports []bool
	h.guard.SetHealthReporter(func(healthy bool) {
		reports = append(reports, healthy)
	})
	h.guard.readTotalUsage = func() (uint64, error) {
		return 0, errors.New("temporary cpu.stat failure")
	}
	h.guard.tick(context.Background())

	h.guard.readTotalUsage = func() (uint64, error) { return 100, nil }
	h.guard.tick(context.Background())
	if want := []bool{false, true}; !reflect.DeepEqual(reports, want) {
		t.Fatalf("health reports = %v, want %v", reports, want)
	}
}

func TestGuardDoesNotChangeTemperatureFromPartialIdleUsageSample(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	h.guard.dec = decider{streak: 1}
	h.guard.prevSampled = time.Now().Add(-time.Second)
	h.guard.prevTotal = 100
	h.guard.prevIdle[string(h.pod.UID)] = 100
	h.guard.readTotalUsage = func() (uint64, error) { return 2_000_000, nil }
	if err := os.Remove(filepath.Join(h.dir, "cpu.stat")); err != nil {
		t.Fatalf("remove pod cpu.stat: %v", err)
	}

	h.guard.tick(context.Background())

	if h.guard.dec.hot || h.guard.dec.streak != 1 {
		t.Fatalf("decider = %+v, want previous cool state untouched by partial accounting", h.guard.dec)
	}
	if !h.guard.prevSampled.IsZero() {
		t.Fatalf("prevSampled = %v, want baseline reset after partial accounting", h.guard.prevSampled)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want no suppression from an incomplete sample", got)
	}
}

func TestGuardDoesNotChangeTemperatureWhenIdlePodSetChanges(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	h.guard.dec = decider{streak: 1}
	h.guard.prevSampled = time.Now().Add(-time.Second)
	h.guard.prevTotal = 100
	h.guard.prevIdle[string(h.pod.UID)] = 100
	h.guard.prevIdle["deleted-idle-pod"] = 100
	h.guard.readTotalUsage = func() (uint64, error) { return 2_000_000, nil }
	h.guard.numCPU = func() int { return 1 }

	h.guard.tick(context.Background())

	if h.guard.dec.hot || h.guard.dec.streak != 1 {
		t.Fatalf("decider = %+v, want previous cool state untouched by pod churn", h.guard.dec)
	}
	if len(h.guard.prevIdle) != 1 {
		t.Fatalf("prevIdle entries = %d, want new one-pod baseline", len(h.guard.prevIdle))
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want no suppression from incomparable samples", got)
	}
}

func TestStableUsageDeltaRejectsCounterReset(t *testing.T) {
	if _, ok := stableUsageDelta(map[string]uint64{"pod": 200}, map[string]uint64{"pod": 100}); ok {
		t.Fatal("stableUsageDelta accepted a decreasing per-pod counter")
	}
}

func TestGuardDoesNotCoolFromInconsistentCounterDeltas(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	h.guard.dec = decider{hot: true, streak: 1}
	h.guard.prevSampled = time.Now().Add(-time.Second)
	h.guard.prevTotal = 1_000
	h.guard.prevIdle[string(h.pod.UID)] = 100
	h.guard.readTotalUsage = func() (uint64, error) { return 1_500, nil }
	h.guard.numCPU = func() int { return 1 }
	if err := os.WriteFile(filepath.Join(h.dir, "cpu.stat"), []byte("usage_usec 1200\n"), 0o644); err != nil {
		t.Fatalf("write pod cpu.stat: %v", err)
	}

	h.guard.tick(context.Background())

	if !h.guard.dec.hot || h.guard.dec.streak != 1 {
		t.Fatalf("decider = %+v, want prior hot state untouched by contradictory deltas", h.guard.dec)
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "10000 100000" {
		t.Fatalf("cpu.max = %q, want existing hot state converged", got)
	}
}

func TestGuardCoolingTransitionDoesNotSuppressThenImmediatelyRestore(t *testing.T) {
	h := newGuardHarness(t, "max 100000")
	h.guard.dec = decider{hot: true, streak: 1}
	h.guard.prevSampled = time.Now().Add(-time.Second)
	h.guard.prevTotal = 1_000
	h.guard.prevIdle[string(h.pod.UID)] = 100
	h.guard.readTotalUsage = func() (uint64, error) { return 1_100, nil }
	h.guard.numCPU = func() int { return 1 }

	h.guard.tick(context.Background())

	if h.guard.dec.hot {
		t.Fatal("guard remained hot after the second consecutive cool sample")
	}
	if got := readGuardKnob(t, h.dir, "cpu.max"); got != "max 100000" {
		t.Fatalf("cpu.max = %q, want untouched on the cooling transition", got)
	}
	if got := h.apiPod(t).Annotations[annotations.GuardStateKey]; got != "" {
		t.Fatalf("guard marker = %q, want no transient ownership", got)
	}
	if got := metricCount(t, h.registry, "cpu_tier_apply_total"); got != 0 {
		t.Fatalf("cpu_tier_apply_total = %v, want no transient suppress/restore outcomes", got)
	}
}

func TestPersistedStateRejectsNonCanonicalCPUValues(t *testing.T) {
	for _, state := range []persistedState{
		{Version: 1, Knob: "cpu.max", Restore: "max\t100000", Suppressed: "10000 100000"},
		{Version: 1, Knob: "cpu.max", Restore: "max 100000", Suppressed: "010000 100000"},
		{Version: 1, Knob: "cpu.max", Restore: "max 100000", Suppressed: "10000\n100000"},
	} {
		if err := validateState(state); err == nil {
			t.Fatalf("validateState(%+v) = nil, want non-canonical marker rejected", state)
		}
	}
}
