package agent

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/azalio/cpi-idle-operator/internal/annotations"
	"github.com/azalio/cpi-idle-operator/internal/apply"
	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/config"
	"github.com/azalio/cpi-idle-operator/internal/envgate"
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

// fixedReadyGate returns a GateCheckFunc that always reports the
// environment gate as passed, so this test never depends on a real cgroup
// v2 mount underneath it (see GateCheckFunc's doc comment).
func fixedReadyGate(driver cgroup.Driver) GateCheckFunc {
	return func(string, envgate.UnameFunc) (envgate.Result, error) {
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
// syscall wrapper: it must return a non-empty release string on the linux
// host this test always runs on (see /tmp/gotest-linux.sh), the same
// precondition envgate.Check's kernelAtLeast relies on.
func TestRealUnameReturnsRelease(t *testing.T) {
	release, err := realUname()
	if err != nil {
		t.Fatalf("realUname() error = %v", err)
	}
	if release == "" {
		t.Error("realUname() release = \"\", want a non-empty kernel release string")
	}
}
