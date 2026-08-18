package apply

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
	"github.com/azalio/cpi-idle-operator/internal/qos"
)

// testPodWithCPURequest builds a minimal single-container pod carrying a
// requests.cpu value. testPod (apply_test.go) only ever sets a CPU limit,
// never a request, so Revert's tests — which drive qos.RestoreWeight off
// requests.cpu — need their own pod builder.
func testPodWithCPURequest(uid, cpuRequest string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "prod",
			UID:       types.UID(uid),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	if cpuRequest != "" {
		pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpuRequest),
		}
	}
	return pod
}

// TestVC1RevertRestoresMeasuredPair covers VC1 [AC-2]: reverting a pod with
// requests.cpu 500m against the exact measured pair from resolution
// T-005/T-006 (idle 1->0 leaves weight at the kernel default until this
// package restores it) must leave cpu.idle==0 and cpu.weight==20 on disk.
// It runs through the production Writer (NewApplier) against a real
// t.TempDir() fixture, so the final values are read from the fixture
// itself, not from a fake's journal.
func TestVC1RevertRestoresMeasuredPair(t *testing.T) {
	t.Run("test_vc1_revert_restores_measured_pair", func(t *testing.T) {
		root := t.TempDir()
		const uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"1", "1", "100000 100000", "0")

		pod := testPodWithCPURequest(uid, "500m")
		state := Snapshot{IdleActive: true, Weight: 1, HasQuota: true, Quota: 100000, Burst: 0}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)

		if err := applier.Revert(context.Background(), pod, state); err != nil {
			t.Fatalf("Revert() error = %v", err)
		}

		assertKnobContent(t, dir, KnobCPUIdle, "0")
		assertKnobContent(t, dir, KnobCPUWeight, "20")
	})
}

// TestVC2NoWeightWriteWhileIdle covers VC2 [INV-2]: cpu.weight is never
// written while the snapshot shows cpu.idle==1 — the write log must show
// cpu.idle strictly before cpu.weight — and if the cpu.idle write itself
// fails, cpu.weight is not attempted at all.
func TestVC2NoWeightWriteWhileIdle(t *testing.T) {
	t.Run("test_vc2_no_weight_write_while_idle", func(t *testing.T) {
		root := t.TempDir()
		const uid = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		pod := testPodWithCPURequest(uid, "500m")
		state := Snapshot{IdleActive: true, Weight: 1, HasQuota: true, Quota: 100000, Burst: 0}
		writer := &fakeWriter{}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

		if err := applier.Revert(context.Background(), pod, state); err != nil {
			t.Fatalf("Revert() error = %v", err)
		}

		idleIdx, weightIdx := -1, -1
		for i, call := range writer.calls {
			switch call.name {
			case KnobCPUIdle:
				idleIdx = i
			case KnobCPUWeight:
				weightIdx = i
			}
		}
		if idleIdx == -1 || weightIdx == -1 {
			t.Fatalf("writer.calls = %+v, want both a cpu.idle and a cpu.weight write", writer.calls)
		}
		if idleIdx >= weightIdx {
			t.Errorf("cpu.idle write index = %d, cpu.weight write index = %d, want cpu.idle strictly before cpu.weight", idleIdx, weightIdx)
		}
	})

	// test_vc2_idle_write_failure_skips_weight_write is table-driven across
	// every error class Revert's switch (revert.go) distinguishes: the plan
	// must break before cpu.weight for all of them, not just the EINVAL
	// case that used to be the only one exercised here.
	t.Run("test_vc2_idle_write_failure_skips_weight_write", func(t *testing.T) {
		cases := []struct {
			name    string
			idleErr error
			wantErr bool // Revert()'s return, per revert.go's switch on writeErr
		}{
			{
				name:    "einval",
				idleErr: fmt.Errorf("cgroup: write knob cpu.idle: %w", syscall.EINVAL),
				wantErr: false, // recorded as rejected, not surfaced for a retry loop
			},
			{
				name:    "cgroup_gone",
				idleErr: cgroup.ErrCgroupGone,
				wantErr: false, // pod raced to deletion mid-plan; silent return
			},
			{
				name:    "not_pod_cgroup",
				idleErr: cgroup.ErrNotPodCgroup,
				wantErr: true,
			},
			{
				name:    "generic",
				idleErr: errors.New("write: disk full"),
				wantErr: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				const uid = "cccccccc-cccc-cccc-cccc-cccccccccccc"
				pod := testPodWithCPURequest(uid, "500m")
				state := Snapshot{IdleActive: true, Weight: 1, HasQuota: true, Quota: 100000, Burst: 0}
				writer := &fakeWriter{results: map[string]error{KnobCPUIdle: tc.idleErr}}
				recorder, events, _, _ := newTestObservers("node-a")
				applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

				err := applier.Revert(context.Background(), pod, state)
				if tc.wantErr && err == nil {
					t.Fatalf("Revert() error = nil, want non-nil for idle write error %v", tc.idleErr)
				}
				if !tc.wantErr && err != nil {
					t.Fatalf("Revert() error = %v, want nil for idle write error %v", err, tc.idleErr)
				}

				if len(writer.calls) != 1 {
					t.Fatalf("writer.calls = %+v, want exactly 1 (the rejected cpu.idle write)", writer.calls)
				}
				if writer.calls[0].name != KnobCPUIdle {
					t.Errorf("writer.calls[0].name = %q, want %q", writer.calls[0].name, KnobCPUIdle)
				}
			})
		}
	})
}

// TestVC3RequestChangedWhileIdle covers VC3 [AC-15]: requests.cpu changed
// from 500m (whatever it was when the pod entered idle) to 2 while the pod
// stayed idle. Revert only ever sees pod as it is right now, so the
// restored weight must be computed from the current spec, not the stale
// value.
func TestVC3RequestChangedWhileIdle(t *testing.T) {
	t.Run("test_vc3_revert_uses_current_spec_not_entry_value", func(t *testing.T) {
		root := t.TempDir()
		const uid = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"1", "1", "max 100000", "0")

		// requests.cpu is 2 right now; whatever it was when the pod entered
		// idle (the measured pair uses 500m -> weight 20) is irrelevant —
		// Revert has no way to see it and must not need to.
		pod := testPodWithCPURequest(uid, "2")
		state := Snapshot{IdleActive: true, Weight: 1, HasQuota: false, Burst: 0}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)

		if err := applier.Revert(context.Background(), pod, state); err != nil {
			t.Fatalf("Revert() error = %v", err)
		}

		wantWeight := strconv.FormatUint(qos.RestoreWeight(pod.Spec), 10)
		if wantWeight == "20" {
			t.Fatalf("test setup invalid: requests.cpu=2 must resolve to a different weight than the 500m entry-time pair (20), or this test cannot distinguish a live computation from a stale cache")
		}
		assertKnobContent(t, dir, KnobCPUWeight, wantWeight)
	})
}

// TestVC3NoCachedWeightField covers VC3 [AC-15] structurally: Applier must
// carry no field that could hold a weight or requests.cpu value captured
// at idle entry. A cache would both violate AC-15 (a requests.cpu change
// mid-idle would restore the stale value) and not survive an agent
// restart, while an idle pod does (resolution T-006) — so the fix has to
// be that the field never exists, not that it is refreshed correctly.
func TestVC3NoCachedWeightField(t *testing.T) {
	t.Run("test_vc3_no_cached_weight_field", func(t *testing.T) {
		typ := reflect.TypeOf(Applier{})
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.ToLower(field.Name)
			if strings.Contains(name, "weight") || strings.Contains(name, "request") || strings.Contains(name, "cache") {
				t.Errorf("Applier has field %q (type %s) that looks like a cached weight/requests value; the restored weight must always come from the live pod.Spec passed to Revert, never a value remembered from when the pod entered idle", field.Name, field.Type)
			}
		}
	})
}

// TestVC3RepeatedRevertUsesLiveSpec covers VC3 [AC-15] black-box, closing a
// gap TestVC3NoCachedWeightField cannot: that test's reflect walk only sees
// fields declared on Applier itself, so it is blind to a cache smuggled in
// through a package-level variable (or any state keyed by UID that lives
// outside the struct). It also cannot be caught by TestVC3RequestChangedWhileIdle
// or any other existing test, because every one of them calls Revert exactly
// once per pod UID — a cache that is only ever populated, never yet read
// back, behaves identically to no cache at all.
//
// This test drives one Applier (and, through it, one Writer) through two
// consecutive Revert calls for the *same* pod UID, changing requests.cpu
// between them (500m -> 2). A UID-keyed cache would replay call one's
// weight (20) on call two instead of recomputing it from the live spec.
func TestVC3RepeatedRevertUsesLiveSpec(t *testing.T) {
	t.Run("test_vc3_revert_repeated_call_same_uid_uses_current_spec", func(t *testing.T) {
		root := t.TempDir()
		const uid = "ffffffff-ffff-ffff-ffff-ffffffffffff"
		dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable, uid,
			"1", "1", "100000 100000", "0")

		state := Snapshot{IdleActive: true, Weight: 1, HasQuota: true, Quota: 100000, Burst: 0}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := NewApplier(root, cgroup.DefaultKubepodsName, cgroup.DriverCgroupfs, recorder, events)

		podFirst := testPodWithCPURequest(uid, "500m")
		if err := applier.Revert(context.Background(), podFirst, state); err != nil {
			t.Fatalf("Revert() call 1 error = %v", err)
		}
		assertKnobContent(t, dir, KnobCPUWeight, "20")

		podSecond := testPodWithCPURequest(uid, "2")
		wantWeight := strconv.FormatUint(qos.RestoreWeight(podSecond.Spec), 10)
		if wantWeight == "20" {
			t.Fatalf("test setup invalid: requests.cpu=2 must resolve to a different weight than the call-1 weight (20), or this test cannot distinguish a live computation from a cache hit")
		}

		if err := applier.Revert(context.Background(), podSecond, state); err != nil {
			t.Fatalf("Revert() call 2 error = %v", err)
		}
		assertKnobContent(t, dir, KnobCPUWeight, wantWeight)
	})
}

// TestRevertBurstOnly covers the "only one of the two tiers is being
// removed" edge case named in the subtask's test strategy: cpu.idle is
// already 0 (nothing to clear, so no weight restore is even considered),
// but cpu.max.burst is still active and must still be cleared.
func TestRevertBurstOnly(t *testing.T) {
	t.Run("test_revert_burst_only_no_idle_or_weight_write", func(t *testing.T) {
		root := t.TempDir()
		const uid = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		pod := testPodWithCPURequest(uid, "500m")
		state := Snapshot{IdleActive: false, Weight: 20, HasQuota: true, Quota: 100000, Burst: 100000}
		writer := &fakeWriter{}
		recorder, events, _, _ := newTestObservers("node-a")
		applier := newTestApplier(root, cgroup.DriverCgroupfs, writer, recorder, events)

		if err := applier.Revert(context.Background(), pod, state); err != nil {
			t.Fatalf("Revert() error = %v", err)
		}

		if len(writer.calls) != 1 {
			t.Fatalf("writer.calls = %+v, want exactly 1 (cpu.max.burst)", writer.calls)
		}
		if call := writer.calls[0]; call.name != KnobCPUMaxBurst || call.value != "0" {
			t.Errorf("write = %+v, want {name: %q, value: \"0\"}", call, KnobCPUMaxBurst)
		}
	})
}
