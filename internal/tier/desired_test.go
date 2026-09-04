package tier

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

func TestUnknownTierNoteBoundsAnnotationValue(t *testing.T) {
	// U+10FFFF is valid UTF-8 but not printable, so fmt's %q expands every
	// rune to the longest \Uxxxxxxxx form used by this diagnostic.
	value := strings.Repeat("\U0010FFFF", 1_000)
	_, notes := Desired(podWithAnnotations(map[string]string{annotations.TierKey: value}, ""))
	if len(notes) != 1 || notes[0].Code != NoteUnknownTierValue {
		t.Fatalf("notes = %+v, want one unknown-value note", notes)
	}
	if got := len(notes[0].Message); got > 1024 {
		t.Fatalf("note message has %d bytes, want at most the Kubernetes Event limit", got)
	}
	if !strings.Contains(notes[0].Message, "…") {
		t.Fatalf("note message %q does not mark the truncated value", notes[0].Message)
	}
}

// podWithAnnotations builds a minimal single-container pod carrying annos.
// cpuLimit, when non-empty, sets the container's positive CPU limit; an
// empty cpuLimit leaves limits.cpu unset entirely, matching how the API
// server represents "no limit", not "limit of zero".
func podWithAnnotations(annos map[string]string, cpuLimit string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         "11111111-1111-1111-1111-111111111111",
			Annotations: annos,
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

// TestVC1BurstWithoutLimitIsNote covers VC1 [AC-4]: a pod carrying the
// burst annotation but no limits.cpu must report BurstRequested=true,
// BurstActive=false, and exactly one Note with code NoteNoCPULimit.
// Desired's signature carries no error return at all, so "nil error" from
// the validation criterion holds structurally — there is nothing to check
// beyond the two return values.
func TestVC1BurstWithoutLimitIsNote(t *testing.T) {
	t.Run("test_vc1_burst_without_limit_is_note_not_error", func(t *testing.T) {
		pod := podWithAnnotations(map[string]string{annotations.BurstKey: ""}, "")

		state, notes := Desired(pod)

		if !state.BurstRequested {
			t.Error("BurstRequested = false, want true")
		}
		if state.BurstActive {
			t.Error("BurstActive = true, want false")
		}
		if len(notes) != 1 {
			t.Fatalf("len(notes) = %d, want 1: %+v", len(notes), notes)
		}
		if notes[0].Code != NoteNoCPULimit {
			t.Errorf("notes[0].Code = %q, want %q", notes[0].Code, NoteNoCPULimit)
		}
		// The note code must sit inside observe's closed vocabulary, not a
		// parallel one: this is the "stitching" the subtask requires.
		if notes[0].Code != observe.TierApplyReasonLimitsCPUMissing {
			t.Errorf("notes[0].Code = %q, want observe.TierApplyReasonLimitsCPUMissing", notes[0].Code)
		}
		if notes[0].Message == "" {
			t.Error("notes[0].Message is empty, want a human-readable explanation")
		}
	})
}

// TestVC1MixedContainersNoQuota covers the HIGH defect from code review 004:
// kubelet only sets a pod-cgroup cpu.max quota when every container that
// runs concurrently with the pod's steady state declares a positive
// limits.cpu. Measured on a live stand (kernel 6.17, k8s 1.36.3): a pod
// with two containers where only one carries limits.cpu ends up with
// cpu.max "max" (no quota) on its pod cgroup, exactly like a pod with no
// limits.cpu at all — not the sum of the one limit that is present. A
// mixed pod must therefore report BurstActive=false and a NoteNoCPULimit
// note, not BurstActive=true.
func TestVC1MixedContainersNoQuota(t *testing.T) {
	t.Run("test_vc1_mixed_containers_no_quota", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "11111111-1111-1111-1111-111111111111",
				Annotations: map[string]string{annotations.BurstKey: ""},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "limited",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					},
					{Name: "unlimited"},
				},
			},
		}

		state, notes := Desired(pod)

		if !state.BurstRequested {
			t.Error("BurstRequested = false, want true")
		}
		if state.BurstActive {
			t.Error("BurstActive = true, want false: one container has no limits.cpu, so kubelet leaves the pod cgroup's quota unset")
		}
		if len(notes) != 1 || notes[0].Code != NoteNoCPULimit {
			t.Fatalf("notes = %+v, want exactly one NoteNoCPULimit", notes)
		}
	})

	t.Run("all_containers_limited_sums_to_active", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "11111111-1111-1111-1111-111111111111",
				Annotations: map[string]string{annotations.BurstKey: ""},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "a",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
						},
					},
					{
						Name: "b",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
						},
					},
				},
			},
		}

		state, notes := Desired(pod)

		if !state.BurstActive {
			t.Error("BurstActive = false, want true: every container declares a positive CPU limit")
		}
		if len(notes) != 0 {
			t.Errorf("notes = %+v, want none", notes)
		}
	})
}

func TestPodLevelCPULimitActivatesBurst(t *testing.T) {
	pod := podWithAnnotations(map[string]string{annotations.BurstKey: ""}, "")
	pod.Spec.Resources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}

	state, notes := Desired(pod)
	if !state.BurstActive {
		t.Fatal("BurstActive = false, want true for a positive pod-level CPU limit")
	}
	if len(notes) != 0 {
		t.Fatalf("notes = %+v, want none", notes)
	}
}

// TestHasPositiveCPULimitInitContainers covers the init/sidecar half of the
// same fix: a native sidecar (RestartPolicy Always) and a plain,
// non-restartable init container must both count exactly like a regular
// container missing a limit does. This is measured behavior, not a
// conclusion drawn from reasoning about container lifecycles: measured on a
// live stand (kernel 6.17, k8s 1.36.3), a main container with limits.cpu
// 200m plus a plain init container with no CPU limit produces pod cgroup
// cpu.max "max 100000" — no quota — because the pod cgroup's cpu.max quota
// is computed once at pod-cgroup creation and never recomputed after the
// init phase completes, regardless of whether the init container is still
// running by the time the regular containers start.
func TestHasPositiveCPULimitInitContainers(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways

	t.Run("sidecar_without_limit_blocks_quota", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "11111111-1111-1111-1111-111111111111",
				Annotations: map[string]string{annotations.BurstKey: ""},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []corev1.Container{
					{Name: "sidecar", RestartPolicy: &always},
				},
			},
		}

		state, _ := Desired(pod)

		if state.BurstActive {
			t.Error("BurstActive = true, want false: the native sidecar has no CPU limit, so it blocks the pod-cgroup quota same as a regular container would")
		}
	})

	t.Run("plain_init_container_without_limit_blocks_quota", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:         "11111111-1111-1111-1111-111111111111",
				Annotations: map[string]string{annotations.BurstKey: ""},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
						},
					},
				},
				InitContainers: []corev1.Container{
					{Name: "setup"},
				},
			},
		}

		state, notes := Desired(pod)

		if state.BurstActive {
			t.Error("BurstActive = true, want false: measured on a live stand, a plain init container without a CPU limit leaves the pod cgroup's cpu.max quota unset (\"max\") even though the regular container declares one, so it blocks the quota same as a regular container without a limit would")
		}
		if len(notes) != 1 || notes[0].Code != NoteNoCPULimit {
			t.Errorf("notes = %+v, want one NoteNoCPULimit note", notes)
		}
	})
}

// TestVC2BurstValueIgnored covers VC2 [SC-2]: the burst annotation's value
// is never parsed. A numeric override like "200000" must be handled
// exactly like an empty value, and exactly like any other string — same
// State, same notes.
func TestVC2BurstValueIgnored(t *testing.T) {
	t.Run("test_vc2_burst_value_ignored", func(t *testing.T) {
		values := []string{"", "200000", "true", "garbage"}

		var states []State
		var noteCounts []int
		for _, v := range values {
			pod := podWithAnnotations(map[string]string{annotations.BurstKey: v}, "500m")
			state, notes := Desired(pod)
			states = append(states, state)
			noteCounts = append(noteCounts, len(notes))
		}

		for i := 1; i < len(values); i++ {
			if states[i] != states[0] {
				t.Errorf("burst value %q produced State %+v, want %+v (same as empty value)", values[i], states[i], states[0])
			}
			if noteCounts[i] != noteCounts[0] {
				t.Errorf("burst value %q produced %d notes, want %d", values[i], noteCounts[i], noteCounts[0])
			}
		}

		if !states[0].BurstRequested {
			t.Error("BurstRequested = false with burst annotation present, want true")
		}
		if !states[0].BurstActive {
			t.Error("BurstActive = false with a positive CPU limit present, want true")
		}
		if noteCounts[0] != 0 {
			t.Errorf("notes = %d with a CPU limit present, want 0", noteCounts[0])
		}

		// Same check again, but this time with no CPU limit at all, so the
		// value-independence holds on the "note" branch too, not only the
		// "active" branch.
		var noLimitCounts []int
		var noLimitCodes []observe.TierApplyReason
		for _, v := range values {
			pod := podWithAnnotations(map[string]string{annotations.BurstKey: v}, "")
			_, notes := Desired(pod)
			noLimitCounts = append(noLimitCounts, len(notes))
			if len(notes) == 1 {
				noLimitCodes = append(noLimitCodes, notes[0].Code)
			}
		}
		for i := 1; i < len(values); i++ {
			if noLimitCounts[i] != noLimitCounts[0] {
				t.Errorf("burst value %q (no limit) produced %d notes, want %d", values[i], noLimitCounts[i], noLimitCounts[0])
			}
			if noLimitCodes[i] != noLimitCodes[0] {
				t.Errorf("burst value %q (no limit) produced code %q, want %q", values[i], noLimitCodes[i], noLimitCodes[0])
			}
		}
	})
}

// TestVC3UnknownVsAbsentTier covers VC3: tier=aggressive must give
// IdleRequested=false plus a NoteUnknownTierValue, while an absent tier key
// must give IdleRequested=false with no note at all. The two must be
// distinguishable, not collapsed into the same observable outcome.
func TestVC3UnknownVsAbsentTier(t *testing.T) {
	t.Run("test_vc3_unknown_vs_absent_tier", func(t *testing.T) {
		unknownPod := podWithAnnotations(map[string]string{annotations.TierKey: "aggressive"}, "")
		unknownState, unknownNotes := Desired(unknownPod)

		if unknownState.IdleRequested {
			t.Error("IdleRequested = true for tier=aggressive, want false")
		}
		if len(unknownNotes) != 1 {
			t.Fatalf("len(notes) = %d for tier=aggressive, want 1: %+v", len(unknownNotes), unknownNotes)
		}
		if unknownNotes[0].Code != NoteUnknownTierValue {
			t.Errorf("notes[0].Code = %q, want %q", unknownNotes[0].Code, NoteUnknownTierValue)
		}

		absentPod := podWithAnnotations(map[string]string{}, "")
		absentState, absentNotes := Desired(absentPod)

		if absentState.IdleRequested {
			t.Error("IdleRequested = true with no tier annotation, want false")
		}
		if len(absentNotes) != 0 {
			t.Fatalf("len(notes) = %d with no tier annotation, want 0: %+v", len(absentNotes), absentNotes)
		}

		// Explicit cross-check: the two paths must not be observably
		// identical modulo note count alone (a note vs. no note is the
		// whole point of this criterion).
		if len(unknownNotes) == len(absentNotes) {
			t.Fatal("unknown tier value and absent tier key produced the same note count; the difference must be visible")
		}
	})

	t.Run("nil_annotations_map_behaves_like_absent_key", func(t *testing.T) {
		pod := podWithAnnotations(nil, "")
		state, notes := Desired(pod)
		if state.IdleRequested {
			t.Error("IdleRequested = true with a nil annotations map, want false")
		}
		if len(notes) != 0 {
			t.Fatalf("len(notes) = %d with a nil annotations map, want 0: %+v", len(notes), notes)
		}
	})

	t.Run("empty_string_tier_value_is_not_a_note", func(t *testing.T) {
		// The contract attaches a note only to a non-empty mismatch; a
		// present-but-empty value is neither TierValueIdle nor the
		// "unrecognized non-empty value" case, so it resolves silently.
		pod := podWithAnnotations(map[string]string{annotations.TierKey: ""}, "")
		state, notes := Desired(pod)
		if state.IdleRequested {
			t.Error("IdleRequested = true for an empty tier value, want false")
		}
		if len(notes) != 0 {
			t.Fatalf("len(notes) = %d for an empty tier value, want 0: %+v", len(notes), notes)
		}
	})

	t.Run("wrong_case_tier_value_is_unknown_not_idle", func(t *testing.T) {
		pod := podWithAnnotations(map[string]string{annotations.TierKey: "Idle"}, "")
		state, notes := Desired(pod)
		if state.IdleRequested {
			t.Error(`IdleRequested = true for "Idle" (wrong case), want false`)
		}
		if len(notes) != 1 || notes[0].Code != NoteUnknownTierValue {
			t.Fatalf("notes = %+v, want exactly one NoteUnknownTierValue", notes)
		}
	})
}

// TestVC4BothTiersIndependent covers VC4: a pod carrying both annotations
// at once gets both flags, independently of each other.
func TestVC4BothTiersIndependent(t *testing.T) {
	t.Run("test_vc4_both_tiers_independent", func(t *testing.T) {
		pod := podWithAnnotations(map[string]string{
			annotations.TierKey:  annotations.TierValueIdle,
			annotations.BurstKey: "",
		}, "500m")

		state, notes := Desired(pod)

		if !state.IdleRequested {
			t.Error("IdleRequested = false, want true")
		}
		if !state.BurstRequested {
			t.Error("BurstRequested = false, want true")
		}
		if !state.BurstActive {
			t.Error("BurstActive = false, want true (CPU limit present)")
		}
		if len(notes) != 0 {
			t.Fatalf("len(notes) = %d, want 0: %+v", len(notes), notes)
		}
	})

	t.Run("idle_only_happy_path_has_no_notes", func(t *testing.T) {
		pod := podWithAnnotations(map[string]string{annotations.TierKey: annotations.TierValueIdle}, "")
		state, notes := Desired(pod)
		if !state.IdleRequested {
			t.Error("IdleRequested = false, want true")
		}
		if state.BurstRequested {
			t.Error("BurstRequested = true with no burst annotation, want false")
		}
		if len(notes) != 0 {
			t.Fatalf("len(notes) = %d for tier=idle alone, want 0: %+v", len(notes), notes)
		}
	})
}

// TestDesiredCarriesQoSAndUID checks the part of State that has no
// dedicated VC but is explicitly required by the subtask: QoSClass and UID
// must be populated from the pod so a later cgroup-path computation never
// needs to recompute or re-fetch either.
func TestDesiredCarriesQoSAndUID(t *testing.T) {
	pod := podWithAnnotations(map[string]string{}, "500m")

	state, _ := Desired(pod)

	if want := qos.ClassOf(pod.Spec); state.QoSClass != want {
		t.Errorf("QoSClass = %s, want %s", state.QoSClass, want)
	}
	if state.UID != pod.UID {
		t.Errorf("UID = %s, want %s", state.UID, pod.UID)
	}
}

// TestDesiredPanicsOnNilPod matches this codebase's existing convention
// (observe.EventRecorder, observe.Recorder) of panicking with a clear
// message on a nil *corev1.Pod rather than an unhelpful nil-pointer
// dereference.
func TestDesiredPanicsOnNilPod(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Desired(nil) did not panic, want a panic")
		}
	}()
	Desired(nil)
}
