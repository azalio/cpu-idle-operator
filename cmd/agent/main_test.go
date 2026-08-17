package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/azalio/cpi-idle-operator/internal/agent"
	"github.com/azalio/cpi-idle-operator/internal/annotations"
	"github.com/azalio/cpi-idle-operator/internal/apply"
	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/config"
	"github.com/azalio/cpi-idle-operator/internal/envgate"
)

// --- fixture helpers ---------------------------------------------------

// testPod builds a minimal single-container pod carrying annos, mirroring
// internal/agent's own reconciler_test.go fixture builder (that helper is
// unexported to its package, so this package repeats the small pattern
// rather than importing it).
func testPod(uid, name, namespace string, annos map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             types.UID(uid),
			ResourceVersion: "1",
			Annotations:     annos,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			}},
		},
	}
}

// seedIdlePodCgroup creates a pod cgroup directory already converged to the
// idle tier (cpu.idle=1) under root, so a Reconciler pass over it finds
// nothing to do — the deterministic way to prove a shutdown path calls the
// Applier zero times regardless of exactly when SIGTERM lands relative to
// the reconcile loop, rather than racing a real convergence against a
// sleep.
func seedIdlePodCgroup(t *testing.T, root string, uid string) {
	t.Helper()
	dir, err := cgroup.PodCgroupPath(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid)
	if err != nil {
		t.Fatalf("PodCgroupPath: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	files := map[string]string{
		apply.KnobCPUIdle:     "1",
		apply.KnobCPUWeight:   "1",
		apply.KnobCPUMax:      "max 100000",
		apply.KnobCPUMaxBurst: "0",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

// mustListen binds an ephemeral loopback port so a test can hand Lifecycle
// a pre-bound net.Listener and read back the actual address before Run
// starts.
func mustListen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return l
}

// fixedReadyGate returns a GateCheckFunc that always reports the
// environment gate as passed, for tests that need Lifecycle's ready branch
// without a real cgroup v2 mount underneath it (see
// agent.GateCheckFunc's doc comment for why this indirection exists).
func fixedReadyGate(driver cgroup.Driver) agent.GateCheckFunc {
	return func(string, string, envgate.UnameFunc) (envgate.Result, error) {
		return envgate.Result{Ready: true, Reason: envgate.ReasonOK, Driver: driver}, nil
	}
}

// waitForStatus polls url until it returns want or timeout elapses,
// returning the last response body and status code it saw.
func waitForStatus(t *testing.T, url string, want int, timeout time.Duration) (body string, status int) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body, status = string(data), resp.StatusCode
		if status == want {
			return body, status
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s did not reach status %d within %s (last status=%d, last body=%q, last error=%v)", url, want, timeout, status, body, lastErr)
	return "", 0
}

// waitForMetric polls url until its response body contains want or timeout
// elapses.
func waitForMetric(t *testing.T, url, want string, timeout time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastBody = string(data)
			if strings.Contains(lastBody, want) {
				return lastBody
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never contained %q within %s; last body:\n%s", url, want, timeout, lastBody)
	return ""
}

// readEvent reads one string off rec.Events, failing the test if none
// arrives within timeout.
func readEvent(t *testing.T, rec *record.FakeRecorder, timeout time.Duration) string {
	t.Helper()
	select {
	case e := <-rec.Events:
		return e
	case <-time.After(timeout):
		t.Fatalf("no event received within %s", timeout)
		return ""
	}
}

// journalingApplier implements agent.Applier, recording every call it
// receives instead of touching a filesystem, so a test can assert on the
// exact sequence of Apply/Revert calls a run made -- in particular, that a
// shutdown path makes none at all (INV-4).
type journalingApplier struct {
	mu    sync.Mutex
	calls []string
}

func (j *journalingApplier) Apply(_ context.Context, pod *corev1.Pod) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, "Apply:"+pod.Name)
	return nil
}

func (j *journalingApplier) Revert(_ context.Context, pod *corev1.Pod, _ apply.Snapshot) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, "Revert:"+pod.Name)
	return nil
}

func (j *journalingApplier) snapshot() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.calls...)
}

// --- VC1 -----------------------------------------------------------------

// TestVC1V1NodeStaysNotReady covers VC1 [AC-6]: on a cgroup v1 fixture, the
// process stays alive (never exits, never restarts), readiness reports 503
// with the gate's reason text, cpi_environment_gate_info carries that same
// reason, and the one annotated pod on this node gets exactly one
// EnvironmentUnsupported Event.
func TestVC1V1NodeStaysNotReady(t *testing.T) {
	t.Run("test_vc1_v1_node_stays_not_ready", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "cpu"), 0o755); err != nil {
			t.Fatalf("mkdir cpu: %v", err)
		}

		pod := testPod("11111111-1111-1111-1111-111111111111", "web-1", "prod", map[string]string{
			annotations.TierKey: annotations.TierValueIdle,
		})
		client := fake.NewSimpleClientset(pod)
		fakeRecorder := record.NewFakeRecorder(20)

		metricsListener := mustListen(t)
		healthListener := mustListen(t)

		lc := &agent.Lifecycle{
			Client: client,
			Config: config.Config{
				CgroupRoot:   root,
				KubepodsName: cgroup.DefaultKubepodsName,
				NodeName:     "node-a",
				ResyncPeriod: time.Hour,
				MetricsAddr:  metricsListener.Addr().String(),
				HealthAddr:   healthListener.Addr().String(),
			},
			EventRecorder:   fakeRecorder,
			MetricsListener: metricsListener,
			HealthListener:  healthListener,
		}

		ctx, cancel := context.WithCancel(context.Background())
		runErr := make(chan error, 1)
		go func() { runErr <- lc.Run(ctx) }()

		readyURL := "http://" + healthListener.Addr().String() + "/readyz"
		body, _ := waitForStatus(t, readyURL, http.StatusServiceUnavailable, 5*time.Second)
		if !strings.Contains(body, "cgroup_v1") {
			t.Errorf("readyz body = %q, want it to contain %q", body, "cgroup_v1")
		}

		metricsURL := "http://" + metricsListener.Addr().String() + "/metrics"
		waitForMetric(t, metricsURL, `cpi_environment_gate_info{node="node-a",reason="cgroup_v1"} 1`, 5*time.Second)

		event := readEvent(t, fakeRecorder, 5*time.Second)
		if !strings.Contains(event, "EnvironmentUnsupported") {
			t.Fatalf("event = %q, want it to contain %q", event, "EnvironmentUnsupported")
		}
		select {
		case extra := <-fakeRecorder.Events:
			t.Errorf("received a second event %q, want exactly one EnvironmentUnsupported Event for this pod", extra)
		case <-time.After(200 * time.Millisecond):
		}

		select {
		case err := <-runErr:
			t.Fatalf("Run returned early with err=%v; a failed gate must keep the process alive, not exit or restart", err)
		default:
		}

		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("Run() error after shutdown = %v, want nil", err)
		}
	})
}

// --- VC3 -------------------------------------------------------------------

// TestVC3ShutdownWritesNothing covers VC3 [INV-4]: a pod already converged
// to the idle tier when SIGTERM arrives must see zero Applier calls across
// the entire run, including its shutdown.
func TestVC3ShutdownWritesNothing(t *testing.T) {
	t.Run("test_vc3_shutdown_writes_nothing", func(t *testing.T) {
		const uid = "33333333-3333-3333-3333-333333333333"
		root := t.TempDir()
		seedIdlePodCgroup(t, root, uid)

		pod := testPod(uid, "web-3", "prod", map[string]string{
			annotations.TierKey: annotations.TierValueIdle,
		})
		client := fake.NewSimpleClientset(pod)
		journal := &journalingApplier{}

		metricsListener := mustListen(t)
		healthListener := mustListen(t)

		lc := &agent.Lifecycle{
			Client: client,
			Config: config.Config{
				CgroupRoot:   root,
				KubepodsName: cgroup.DefaultKubepodsName,
				NodeName:     "node-a",
				ResyncPeriod: time.Hour,
				MetricsAddr:  metricsListener.Addr().String(),
				HealthAddr:   healthListener.Addr().String(),
			},
			EventRecorder:   record.NewFakeRecorder(20),
			Applier:         journal,
			MetricsListener: metricsListener,
			HealthListener:  healthListener,
			GateCheck:       fixedReadyGate(cgroup.DriverCgroupfs),
		}

		ctx, cancel := context.WithCancel(context.Background())
		runErr := make(chan error, 1)
		go func() { runErr <- lc.Run(ctx) }()

		readyURL := "http://" + healthListener.Addr().String() + "/readyz"
		waitForStatus(t, readyURL, http.StatusOK, 5*time.Second)

		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("Run() error after shutdown = %v, want nil", err)
		}

		if calls := journal.snapshot(); len(calls) != 0 {
			t.Errorf("applier calls across the run = %v, want none: a pod already in the idle tier must never see an Apply or Revert call, on shutdown or otherwise (INV-4)", calls)
		}
	})
}

// TestVC3RevertNeverCalledInCmd is VC3's second half: a static grep over
// every non-test .go file in this directory, confirming cmd/ never calls
// Revert at all -- this package only ever wires internal/agent.Lifecycle
// together, and reconciliation (including every sanctioned Revert call)
// lives entirely in internal/agent.Reconciler's own loop, not here.
func TestVC3RevertNeverCalledInCmd(t *testing.T) {
	t.Run("grep_no_revert_call_in_cmd", func(t *testing.T) {
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatalf("ReadDir(.) error = %v", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v", name, err)
			}
			if strings.Contains(string(data), "Revert(") {
				t.Errorf("%s calls Revert(...); cmd/ must never call Revert on any path (INV-4 forbids rollback on shutdown)", name)
			}
		}
	})
}

// --- VC4 -------------------------------------------------------------------

// TestVC4ReadinessAfterCacheSync covers VC4: readiness reports 200 only
// once both the environment gate passed and the informer's cache has
// completed its initial sync.
func TestVC4ReadinessAfterCacheSync(t *testing.T) {
	t.Run("health_ready_requires_gate_and_sync", func(t *testing.T) {
		notReported := agent.NewHealth()
		if ready, _ := notReported.Ready(); ready {
			t.Fatalf("Ready() = true before any SetGateResult/SetSynced call, want false")
		}

		syncedOnly := agent.NewHealth()
		syncedOnly.SetSynced(true)
		if ready, reason := syncedOnly.Ready(); ready {
			t.Errorf("Ready() = true with the gate never reported passed, want false (reason=%q)", reason)
		}

		gateOnly := agent.NewHealth()
		gateOnly.SetGateResult(true, "ok")
		if ready, reason := gateOnly.Ready(); ready {
			t.Errorf("Ready() = true before cache sync, want false (reason=%q)", reason)
		}
		gateOnly.SetSynced(true)
		if ready, reason := gateOnly.Ready(); !ready {
			t.Errorf("Ready() = false after gate passed and cache synced, want true (reason=%q)", reason)
		}
	})

	t.Run("test_vc4_readiness_after_gate_and_sync", func(t *testing.T) {
		pod := testPod("44444444-4444-4444-4444-444444444444", "web-4", "prod", nil)
		client := fake.NewSimpleClientset(pod)

		metricsListener := mustListen(t)
		healthListener := mustListen(t)

		lc := &agent.Lifecycle{
			Client: client,
			Config: config.Config{
				CgroupRoot:   t.TempDir(),
				KubepodsName: cgroup.DefaultKubepodsName,
				NodeName:     "node-a",
				ResyncPeriod: time.Hour,
				MetricsAddr:  metricsListener.Addr().String(),
				HealthAddr:   healthListener.Addr().String(),
			},
			EventRecorder:   record.NewFakeRecorder(20),
			MetricsListener: metricsListener,
			HealthListener:  healthListener,
			GateCheck:       fixedReadyGate(cgroup.DriverCgroupfs),
		}

		ctx, cancel := context.WithCancel(context.Background())
		runErr := make(chan error, 1)
		go func() { runErr <- lc.Run(ctx) }()

		readyURL := "http://" + healthListener.Addr().String() + "/readyz"
		waitForStatus(t, readyURL, http.StatusOK, 5*time.Second)

		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("Run() error after shutdown = %v, want nil", err)
		}
	})
}
