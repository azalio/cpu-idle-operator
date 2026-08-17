package qos

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// containerResources builds a ResourceRequirements from the four values
// that matter to QoS. An empty string means "field not set" — not "set to
// zero" — matching how kubectl/the API server represent an absent request
// or limit.
func containerResources(requestCPU, requestMem, limitCPU, limitMem string) corev1.ResourceRequirements {
	req := corev1.ResourceList{}
	if requestCPU != "" {
		req[corev1.ResourceCPU] = resource.MustParse(requestCPU)
	}
	if requestMem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(requestMem)
	}
	lim := corev1.ResourceList{}
	if limitCPU != "" {
		lim[corev1.ResourceCPU] = resource.MustParse(limitCPU)
	}
	if limitMem != "" {
		lim[corev1.ResourceMemory] = resource.MustParse(limitMem)
	}

	rr := corev1.ResourceRequirements{}
	if len(req) > 0 {
		rr.Requests = req
	}
	if len(lim) > 0 {
		rr.Limits = lim
	}
	return rr
}

// TestVC3QoSFromSpec covers VC3: ClassOf must derive Guaranteed, Burstable
// and BestEffort purely from spec, and VerifyAgainstStatus must flag a
// disagreement with a non-empty status without ever changing the computed
// class — while an empty status is not treated as a disagreement at all.
func TestVC3QoSFromSpec(t *testing.T) {
	guaranteed := corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "app", Resources: containerResources("500m", "256Mi", "500m", "256Mi")},
		},
	}
	burstable := corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "app", Resources: containerResources("100m", "128Mi", "500m", "256Mi")},
		},
	}
	bestEffort := corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{}},
		},
	}
	guaranteedMultiContainer := corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "app", Resources: containerResources("100m", "128Mi", "100m", "128Mi")},
			{Name: "sidecar", Resources: containerResources("50m", "64Mi", "50m", "64Mi")},
		},
	}
	burstablePartialLimits := corev1.PodSpec{
		// One container has full requests==limits, the other only sets a
		// CPU limit with no memory limit at all: kubelet demotes the whole
		// pod to Burstable even though the fully-specified container looks
		// Guaranteed in isolation.
		Containers: []corev1.Container{
			{Name: "app", Resources: containerResources("100m", "128Mi", "100m", "128Mi")},
			{Name: "sidecar", Resources: containerResources("", "", "50m", "")},
		},
	}

	classTests := []struct {
		name string
		spec corev1.PodSpec
		want Class
	}{
		{"guaranteed_single_container_requests_equal_limits", guaranteed, Guaranteed},
		{"burstable_requests_below_limits", burstable, Burstable},
		{"best_effort_no_requests_no_limits", bestEffort, BestEffort},
		{"guaranteed_multi_container", guaranteedMultiContainer, Guaranteed},
		{"burstable_one_container_missing_a_limit", burstablePartialLimits, Burstable},
	}
	for _, tc := range classTests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassOf(tc.spec); got != tc.want {
				t.Fatalf("ClassOf() = %s, want %s", got, tc.want)
			}
		})
	}

	verifyTests := []struct {
		name         string
		computed     Class
		status       corev1.PodQOSClass
		wantMismatch bool
	}{
		{"matching_status_no_mismatch", Guaranteed, corev1.PodQOSGuaranteed, false},
		{"empty_status_not_a_mismatch", Guaranteed, "", false},
		{"disagreeing_status_is_mismatch", Guaranteed, corev1.PodQOSBurstable, true},
	}
	for _, tc := range verifyTests {
		t.Run(tc.name, func(t *testing.T) {
			mismatch, message := VerifyAgainstStatus(tc.computed, tc.status)
			if mismatch != tc.wantMismatch {
				t.Fatalf("VerifyAgainstStatus() mismatch = %v, want %v", mismatch, tc.wantMismatch)
			}
			if tc.wantMismatch && message == "" {
				t.Fatal("VerifyAgainstStatus() returned mismatch=true with an empty log message")
			}
			if !tc.wantMismatch && message != "" {
				t.Fatalf("VerifyAgainstStatus() returned a non-empty message %q for a non-mismatch", message)
			}
		})
	}

	t.Run("mismatch_does_not_change_the_computed_class", func(t *testing.T) {
		computed := ClassOf(guaranteed)
		mismatch, _ := VerifyAgainstStatus(computed, corev1.PodQOSBurstable)
		if !mismatch {
			t.Fatal("expected VerifyAgainstStatus to report a mismatch for Guaranteed vs status Burstable")
		}
		if computed != Guaranteed {
			t.Fatalf("computed class changed after VerifyAgainstStatus: got %s, want %s", computed, Guaranteed)
		}
		if again := ClassOf(guaranteed); again != computed {
			t.Fatalf("ClassOf is not stable across calls: got %s, want %s", again, computed)
		}
	})
}
