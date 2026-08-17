package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/azalio/cpi-idle-operator/internal/apply"
	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/config"
	"github.com/azalio/cpi-idle-operator/internal/qos"
)

// revertAllTestPod builds a minimal single-container pod with a caller-
// chosen name/UID and, optionally, a CPU request. reconciler_test.go's own
// testPod always reuses the same namespace/name ("prod/web-1"), which
// collides when a fixture needs several distinct pods coexisting in one
// fake clientset (VC1's three-pod node, VC2's two-pod node) — this helper
// exists for that case.
func revertAllTestPod(name, uid, cpuRequest string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "prod",
			UID:       types.UID(uid),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
			NodeName:   "node-a",
		},
	}
	if cpuRequest != "" {
		pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpuRequest),
		}
	}
	return pod
}

// assertKnobContent reads dir/knob and fails the test if it does not equal
// want. Unlike reconciler_test.go's waitForKnobContent, RunRevertAll is
// synchronous, so a direct read (no polling) is enough here.
func assertKnobContent(t *testing.T, dir, knob, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, knob))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(dir, knob), err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", filepath.Join(dir, knob), string(got), want)
	}
}

// TestVC1RevertAllClearsNode covers VC1 [AC-7]: a node with three pods —
// one carrying only an active idle tier, one carrying only an active burst
// tier, one carrying both — must have every active tier cleared and every
// touched weight restored from the pod's current spec, with RunRevertAll
// returning nil (exit 0).
func TestVC1RevertAllClearsNode(t *testing.T) {
	t.Run("test_vc1_revert_all_clears_node_exit_zero", func(t *testing.T) {
		root := t.TempDir()

		const (
			idleUID  = "11111111-1111-1111-1111-111111111111"
			burstUID = "22222222-2222-2222-2222-222222222222"
			bothUID  = "33333333-3333-3333-3333-333333333333"
		)

		// idle-only: cpu.idle=1, no quota configured (HasQuota=false), so
		// there is nothing for cpu.max.burst to act on.
		idleDir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, idleUID,
			"1", "1", "max 100000", "0")
		// burst-only: cpu.idle already 0 (nothing to restore a weight for),
		// cpu.max.burst active at the pod's own quota.
		burstDir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, burstUID,
			"0", "20", "100000 100000", "100000")
		// both: cpu.idle=1 and cpu.max.burst active together.
		bothDir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, bothUID,
			"1", "1", "100000 100000", "100000")

		idlePod := revertAllTestPod("idle-only", idleUID, "500m")
		burstPod := revertAllTestPod("burst-only", burstUID, "500m")
		bothPod := revertAllTestPod("both-tiers", bothUID, "500m")

		client := fake.NewSimpleClientset(idlePod, burstPod, bothPod)

		cfg := config.Config{NodeName: "node-a", CgroupRoot: root}
		var out bytes.Buffer
		opts := RevertAllOptions{
			Client:        client,
			GateCheck:     fixedReadyGate(cgroup.DriverCgroupfs),
			EventRecorder: record.NewFakeRecorder(100),
			Out:           &out,
		}

		if err := RunRevertAll(context.Background(), cfg, opts); err != nil {
			t.Fatalf("RunRevertAll() error = %v, want nil", err)
		}

		wantWeight := strconv.FormatUint(qos.RestoreWeight(idlePod.Spec), 10)

		assertKnobContent(t, idleDir, apply.KnobCPUIdle, "0")
		assertKnobContent(t, idleDir, apply.KnobCPUWeight, wantWeight)

		assertKnobContent(t, burstDir, apply.KnobCPUMaxBurst, "0")
		assertKnobContent(t, burstDir, apply.KnobCPUIdle, "0")
		// burst-only never had cpu.idle active, so Revert never restores a
		// weight for it (apply/revert.go's revertPlan): it must stay at
		// whatever seedPodCgroup wrote.
		assertKnobContent(t, burstDir, apply.KnobCPUWeight, "20")

		assertKnobContent(t, bothDir, apply.KnobCPUIdle, "0")
		assertKnobContent(t, bothDir, apply.KnobCPUWeight, wantWeight)
		assertKnobContent(t, bothDir, apply.KnobCPUMaxBurst, "0")

		table := out.String()
		for _, want := range []string{"prod/idle-only", "prod/burst-only", "prod/both-tiers", "idle", "burst", "ok"} {
			if !bytes.Contains([]byte(table), []byte(want)) {
				t.Errorf("printed table = %q, want it to contain %q", table, want)
			}
		}
	})
}

// revertAllFakeApplier is a call-journaling Applier double for
// TestVC2PartialFailureNonzeroExit: it lets the test force exactly one
// pod's Revert call to fail while every other pod's succeeds, without
// depending on a real cgroup write producing a kernel-level error to
// exercise the "one bad pod must not stop the pass" behavior.
type revertAllFakeApplier struct {
	mu        sync.Mutex
	failOnUID types.UID
	failErr   error
	calls     []types.UID
}

func (f *revertAllFakeApplier) Apply(context.Context, *corev1.Pod) error {
	return nil
}

func (f *revertAllFakeApplier) Revert(_ context.Context, pod *corev1.Pod, _ apply.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pod.UID)
	if pod.UID == f.failOnUID {
		return f.failErr
	}
	return nil
}

func (f *revertAllFakeApplier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestVC2PartialFailureNonzeroExit covers VC2: one pod's revert failing
// must make RunRevertAll return a non-nil error, but must not stop it from
// attempting every other pod on the node.
func TestVC2PartialFailureNonzeroExit(t *testing.T) {
	t.Run("test_vc2_partial_failure_nonzero_exit", func(t *testing.T) {
		root := t.TempDir()

		const (
			okUID   = "44444444-4444-4444-4444-444444444444"
			failUID = "55555555-5555-5555-5555-555555555555"
		)

		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, okUID,
			"1", "1", "max 100000", "0")
		seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, failUID,
			"1", "1", "max 100000", "0")

		okPod := revertAllTestPod("ok-pod", okUID, "500m")
		failPod := revertAllTestPod("fail-pod", failUID, "500m")

		client := fake.NewSimpleClientset(okPod, failPod)

		fakeApplier := &revertAllFakeApplier{
			failOnUID: types.UID(failUID),
			failErr:   errors.New("simulated write failure"),
		}

		cfg := config.Config{NodeName: "node-a", CgroupRoot: root}
		opts := RevertAllOptions{
			Client:    client,
			GateCheck: fixedReadyGate(cgroup.DriverCgroupfs),
			Applier:   fakeApplier,
			Out:       &bytes.Buffer{},
		}

		err := RunRevertAll(context.Background(), cfg, opts)
		if err == nil {
			t.Fatal("RunRevertAll() error = nil, want non-nil: one pod's revert failed")
		}

		if got := fakeApplier.callCount(); got != 2 {
			t.Errorf("Applier.Revert was called %d times, want 2: a failing pod must not stop the pass over the rest", got)
		}
	})
}

// freeAddr binds an ephemeral loopback port, closes it immediately, and
// returns its address string — a port a test can hand to code under test
// and later re-bind itself to prove nothing else claimed it in between.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// TestVC3RevertAllIsOneshot covers VC3: RunRevertAll must never listen on
// the metrics/health addresses configured in Config, and must never open a
// watch against the API server — only the single List call.
func TestVC3RevertAllIsOneshot(t *testing.T) {
	t.Run("test_vc3_revert_all_is_oneshot", func(t *testing.T) {
		root := t.TempDir()

		client := fake.NewSimpleClientset()
		var watchOpened bool
		client.PrependWatchReactor("*", func(action k8stesting.Action) (bool, watch.Interface, error) {
			watchOpened = true
			t.Errorf("RunRevertAll opened a watch against the fake clientset: %+v", action)
			return false, nil, nil
		})

		metricsAddr := freeAddr(t)
		healthAddr := freeAddr(t)

		cfg := config.Config{
			NodeName:    "node-a",
			CgroupRoot:  root,
			MetricsAddr: metricsAddr,
			HealthAddr:  healthAddr,
		}
		opts := RevertAllOptions{
			Client:    client,
			GateCheck: fixedReadyGate(cgroup.DriverCgroupfs),
			Out:       &bytes.Buffer{},
		}

		if err := RunRevertAll(context.Background(), cfg, opts); err != nil {
			t.Fatalf("RunRevertAll() error = %v, want nil for an empty node", err)
		}

		if watchOpened {
			t.Error("watchOpened = true, want false: --revert-all must be a single List, never a watch")
		}

		for _, addr := range []string{metricsAddr, healthAddr} {
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Errorf("port %s is not free after RunRevertAll: %v (a listener must have leaked)", addr, err)
				continue
			}
			_ = l.Close()
		}
	})
}
