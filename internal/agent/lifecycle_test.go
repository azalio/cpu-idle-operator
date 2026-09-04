package agent

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/apply"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/config"
	"github.com/azalio/cpu-idle-operator/internal/envgate"
	"github.com/azalio/cpu-idle-operator/internal/observe"
)

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

type failingListener struct {
	err error
}

func (l *failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *failingListener) Close() error              { return nil }
func (l *failingListener) Addr() net.Addr            { return staticAddr("failing-listener") }

type staticAddr string

func (a staticAddr) Network() string { return "test" }
func (a staticAddr) String() string  { return string(a) }

// fixedReadyGate returns a GateCheckFunc that always reports the
// environment gate as passed, so this test never depends on a real cgroup
// v2 mount underneath it (see GateCheckFunc's doc comment).
func fixedReadyGate(driver cgroup.Driver) GateCheckFunc {
	return func(string, string, envgate.UnameFunc) (envgate.Result, error) {
		return envgate.Result{Ready: true, Reason: envgate.ReasonOK, Driver: driver}, nil
	}
}

// fsEntry is a byte-level snapshot of one filesystem entry: its identity
// (path + mode) and, for files, its content and mtime. Comparing slices of
// this before and after a Lifecycle.Run shutdown is what makes VC2 an
// actual proof of "the stop wrote nothing", not just "Run returned no
// error" -- mirrors internal/envgate/gate_test.go's own snapshotTree,
// which is unexported to that package and so is repeated here rather than
// imported.
type fsEntry struct {
	path    string
	mode    fs.FileMode
	content string
	modTime time.Time
}

func snapshotTree(t *testing.T, root string) []fsEntry {
	t.Helper()
	var entries []fsEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		entry := fsEntry{path: p, mode: info.Mode(), modTime: info.ModTime()}
		if !d.IsDir() {
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			entry.content = string(data)
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot tree %s: %v", root, err)
	}
	return entries
}

// TestVC2RestartConvergesStopChangesNothing covers VC2 [AC-5]: a byte-level
// snapshot of the cgroup fixture tree, taken right before SIGTERM and right
// after Lifecycle.Run returns, must be identical -- the stop itself writes
// nothing. A second Lifecycle.Run over the same fixture, started after an
// external actor drifted the pod's cgroup back out of sync with its
// annotation while the agent was down, must then reconverge it: this is
// the idempotent full-node reconciliation a restart performs, covering the
// first run, a crash, and an upgrade with the same code path.
func TestVC2RestartConvergesStopChangesNothing(t *testing.T) {
	t.Run("test_vc2_restart_converges_stop_changes_nothing", func(t *testing.T) {
		root := t.TempDir()
		const uid = "22222222-2222-2222-2222-222222222222"
		dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"0", "20", "100000 100000", "0")

		pod := testPod(uid, "500m", map[string]string{
			annotations.TierKey: annotations.TierValueIdle,
		})
		client := fake.NewSimpleClientset(pod)

		newLifecycle := func() *Lifecycle {
			metricsListener := mustListen(t)
			healthListener := mustListen(t)
			return &Lifecycle{
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
				GateCheck:       fixedReadyGate(cgroup.DriverCgroupfs),
				MetricsListener: metricsListener,
				HealthListener:  healthListener,
			}
		}

		// --- start #1: converge cpu.idle to 1 -------------------------
		lc1 := newLifecycle()
		ctx1, cancel1 := context.WithCancel(context.Background())
		runErr1 := make(chan error, 1)
		go func() { runErr1 <- lc1.Run(ctx1) }()

		waitForKnobContent(t, dir, apply.KnobCPUIdle, "1", 5*time.Second)

		snapshotBeforeStop := snapshotTree(t, root)

		// --- SIGTERM: stop must write nothing ---------------------------
		cancel1()
		if err := <-runErr1; err != nil {
			t.Fatalf("Run() (first start) error after shutdown = %v, want nil", err)
		}

		snapshotAfterStop := snapshotTree(t, root)
		if !reflect.DeepEqual(snapshotBeforeStop, snapshotAfterStop) {
			t.Fatalf("cgroup tree under %s changed across shutdown:\nbefore: %+v\nafter:  %+v", root, snapshotBeforeStop, snapshotAfterStop)
		}

		// --- simulate drift while the agent was down ---------------------
		// Intent: an external actor (a manual kubectl exec, or a crash
		// mid-write in a previous life) reset cpu.idle while nothing was
		// watching it. The pod's annotation still requests idle, so a
		// restart's initial reconciliation must notice and correct it.
		if err := os.WriteFile(filepath.Join(dir, apply.KnobCPUIdle), []byte("0"), 0o644); err != nil {
			t.Fatalf("inject drift: %v", err)
		}

		// --- start #2: restart must reconverge ----------------------------
		lc2 := newLifecycle()
		ctx2, cancel2 := context.WithCancel(context.Background())
		runErr2 := make(chan error, 1)
		go func() { runErr2 <- lc2.Run(ctx2) }()

		waitForKnobContent(t, dir, apply.KnobCPUIdle, "1", 5*time.Second)

		cancel2()
		if err := <-runErr2; err != nil {
			t.Fatalf("Run() (restart) error after shutdown = %v, want nil", err)
		}
	})
}

// TestRealUnameReturnsRelease is a basic sanity check on realUname's
// syscall wrapper: it must return a non-empty release string on the current
// host. Linux production uses unix.ByteSliceToString; developer platforms use
// the equivalent byte conversion in uname_unsupported.go.
func TestRealUnameReturnsRelease(t *testing.T) {
	release, err := realUname()
	if err != nil {
		t.Fatalf("realUname() error = %v", err)
	}
	if release == "" {
		t.Error("realUname() release = \"\", want a non-empty kernel release string")
	}
}

func TestLifecycleDoesNotProcessGuardMarkersWhenGuardIsDisabled(t *testing.T) {
	root := t.TempDir()
	const uid = "56565656-5656-5656-5656-565656565656"
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBestEffort, uid,
		"0", "20", "10000 100000", "0")
	pod := testPod(uid, "500m", map[string]string{
		annotations.TierKey:       annotations.TierValueIdle,
		annotations.GuardStateKey: `{"version":1,"knob":"cpu.max","restore":"max 100000","suppressed":"10000 100000"}`,
	})
	pod.Spec.Containers[0].Resources.Limits = nil
	client := fake.NewSimpleClientset(pod)
	metricsListener := mustListen(t)
	healthListener := mustListen(t)
	lc := &Lifecycle{
		Client: client,
		Config: config.Config{
			CgroupRoot:   root,
			KubepodsName: cgroup.DefaultKubepodsName,
			NodeName:     "node-a",
			ResyncPeriod: time.Hour,
			MetricsAddr:  metricsListener.Addr().String(),
			HealthAddr:   healthListener.Addr().String(),
			GuardHigh:    0,
		},
		EventRecorder:   record.NewFakeRecorder(20),
		GateCheck:       fixedReadyGate(cgroup.DriverCgroupfs),
		MetricsListener: metricsListener,
		HealthListener:  healthListener,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- lc.Run(ctx) }()
	// Waiting for ordinary tier convergence proves startup passed cache sync
	// and entered the reconciler. The disabled guard must still leave both
	// its live cpu.max state and tenant-controlled marker untouched.
	waitForKnobContent(t, dir, apply.KnobCPUIdle, "1", 5*time.Second)
	waitForKnobContent(t, dir, apply.KnobCPUMax, "10000 100000", time.Second)
	current, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got := current.Annotations[annotations.GuardStateKey]; got == "" {
		t.Fatal("disabled guard cleared an ownership marker")
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Lifecycle.Run() error = %v", err)
	}
}

func TestLifecycleRecoversGuardStateWhenEnabled(t *testing.T) {
	root := t.TempDir()
	const uid = "57575757-5757-5757-5757-575757575757"
	dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBestEffort, uid,
		"0", "20", "10000 100000", "0")
	pod := testPod(uid, "500m", map[string]string{
		annotations.GuardStateKey: `{"version":1,"knob":"cpu.max","restore":"max 100000","suppressed":"10000 100000"}`,
	})
	pod.Spec.Containers[0].Resources.Limits = nil
	client := fake.NewSimpleClientset(pod)
	metricsListener := mustListen(t)
	healthListener := mustListen(t)
	lc := &Lifecycle{
		Client: client,
		Config: config.Config{
			CgroupRoot:   root,
			KubepodsName: cgroup.DefaultKubepodsName,
			NodeName:     "node-a",
			ResyncPeriod: time.Hour,
			MetricsAddr:  metricsListener.Addr().String(),
			HealthAddr:   healthListener.Addr().String(),
			GuardHigh:    0.70,
			GuardLow:     0.60,
			GuardPeriod:  time.Hour,
			GuardFloor:   "10000 100000",
		},
		EventRecorder:   record.NewFakeRecorder(20),
		GateCheck:       fixedReadyGate(cgroup.DriverCgroupfs),
		MetricsListener: metricsListener,
		HealthListener:  healthListener,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- lc.Run(ctx) }()
	waitForKnobContent(t, dir, apply.KnobCPUMax, "max 100000", 5*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get pod: %v", err)
		}
		if current.Annotations[annotations.GuardStateKey] == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guard marker was not cleared: %q", current.Annotations[annotations.GuardStateKey])
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Lifecycle.Run() error = %v", err)
	}
}

func TestLifecycleReturnsWhenEssentialHTTPServerFails(t *testing.T) {
	wantErr := errors.New("accept failed")
	healthListener := mustListen(t)
	lc := &Lifecycle{
		Client: fake.NewSimpleClientset(),
		Config: config.Config{
			NodeName:     "node-a",
			ResyncPeriod: time.Hour,
			MetricsAddr:  "failing-listener",
			HealthAddr:   healthListener.Addr().String(),
		},
		GateCheck: func(string, string, envgate.UnameFunc) (envgate.Result, error) {
			return envgate.Result{Ready: false, Reason: envgate.ReasonDriverUnknown}, nil
		},
		EventRecorder:   record.NewFakeRecorder(10),
		MetricsListener: &failingListener{err: wantErr},
		HealthListener:  healthListener,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := lc.Run(ctx)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Lifecycle.Run() error = %v, want wrapped %v", err, wantErr)
	}
}

func TestDegradedLifecycleReportsUnknownTierAndAllowsReappearance(t *testing.T) {
	const uid = "67676767-6767-6767-6767-676767676767"
	pod := testPod(uid, "", map[string]string{annotations.TierKey: "future-tier"})
	pod.Spec.NodeName = "node-a"
	client := fake.NewSimpleClientset(pod)
	events := record.NewFakeRecorder(20)
	informer, err := NewInformer(client, "node-a", time.Hour)
	if err != nil {
		t.Fatalf("NewInformer: %v", err)
	}
	lc := &Lifecycle{Config: config.Config{NodeName: "node-a"}}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- lc.runDegraded(ctx, informer, observe.NewEventRecorder(events)) }()
	waitForEventReason(t, events, string(observe.ReasonTierValueUnknown), 5*time.Second)

	current, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	current.Annotations = nil
	if _, err := client.CoreV1().Pods(pod.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("clear unknown tier: %v", err)
	}
	waitForCachedTierValue(t, informer, pod.Namespace, pod.Name, "", 5*time.Second)
	current, err = client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cleared pod: %v", err)
	}
	current.Annotations = map[string]string{annotations.TierKey: "future-tier"}
	if _, err := client.CoreV1().Pods(pod.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("restore unknown tier: %v", err)
	}
	waitForEventReason(t, events, string(observe.ReasonTierValueUnknown), 5*time.Second)

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Lifecycle.Run() error = %v", err)
	}
}

func waitForCachedTierValue(t *testing.T, informer *Informer, namespace, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := informer.Lister().Pods(namespace).Get(name)
		if err == nil && pod.Annotations[annotations.TierKey] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cached tier value did not become %q within %s", want, timeout)
}

func waitForEventReason(t *testing.T, events *record.FakeRecorder, reason string, timeout time.Duration) string {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-events.Events:
			if strings.Contains(event, reason) {
				return event
			}
		case <-deadline.C:
			t.Fatalf("no Event with reason %q within %s", reason, timeout)
		}
	}
}
