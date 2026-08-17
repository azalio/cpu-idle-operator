package qos

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// containerWithCPURequest returns a container named name with requests.cpu
// set to cpu, or with no CPU request at all when cpu is "".
func containerWithCPURequest(name, cpu string) corev1.Container {
	c := corev1.Container{Name: name}
	if cpu != "" {
		c.Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu),
		}
	}
	return c
}

// nativeSidecar returns an init container named name with requests.cpu set
// to cpu and RestartPolicy Always, marking it as a native sidecar that
// kubelet runs concurrently with the pod's regular containers for the
// pod's whole lifetime rather than to completion before them.
func nativeSidecar(name, cpu string) corev1.Container {
	c := containerWithCPURequest(name, cpu)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// TestVC1NoRequestWeightOne covers VC1: a pod with no requests.cpu
// anywhere must restore to weight 1, kubelet's minimum-shares floor.
func TestVC1NoRequestWeightOne(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{containerWithCPURequest("app", "")},
	}
	if got := RestoreWeight(spec); got != 1 {
		t.Fatalf("RestoreWeight() = %d, want 1 for a pod with no requests.cpu", got)
	}
}

// TestVC2MeasuredPair500m20 covers VC2: requests.cpu=500m must restore to
// weight 20, the pair measured on the reference stand (see T-005.md) and
// pinned as the acceptance-level regression case.
func TestVC2MeasuredPair500m20(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{containerWithCPURequest("app", "500m")},
	}
	const wantWeight = 20
	if got := RestoreWeight(spec); got != wantWeight {
		t.Fatalf("RestoreWeight() = %d, want %d for requests.cpu=500m", got, wantWeight)
	}
}

// TestRestoreWeightFormulaTable pins the kubelet shares-to-weight formula
// against checkpoints verified independently of the reference stand pair.
// Of these, only 1539m -> 60 actually guards weightDenominator: at
// shares=1575, (shares-2)*weightNumerator=15,728,427, which divides to 60
// under the correct 262142 but to 61 under the naive off-by-one twin
// 262140 — the two denominators land in different integer-division buckets
// right at that remainder. Every other checkpoint here (0m, 1m, 100m, 500m,
// 1000m, 2000m, 4000m) produces the identical weight under both
// denominators, so on their own they only guard weightNumerator and the
// shares formula, not weightDenominator. This is exactly how 262140 slipped
// through the first version of the stand script undetected.
func TestRestoreWeightFormulaTable(t *testing.T) {
	tests := []struct {
		milliCPU string
		want     uint64
	}{
		{"0m", 1},
		{"1m", 1},
		{"100m", 4},
		{"500m", 20},
		{"1000m", 39},
		{"1539m", 60},
		{"2000m", 79},
		{"4000m", 157},
	}
	for _, tc := range tests {
		t.Run(tc.milliCPU, func(t *testing.T) {
			spec := corev1.PodSpec{
				Containers: []corev1.Container{containerWithCPURequest("app", tc.milliCPU)},
			}
			if got := RestoreWeight(spec); got != tc.want {
				t.Fatalf("RestoreWeight(%s) = %d, want %d", tc.milliCPU, got, tc.want)
			}
		})
	}
}

// TestWeightTableMultiContainer covers the multi-container edge cases:
// regular container requests sum, while an init container's request is
// maxed against that sum rather than added to it, since init containers
// run sequentially and never overlap with regular containers.
func TestWeightTableMultiContainer(t *testing.T) {
	tests := []struct {
		name       string
		spec       corev1.PodSpec
		wantWeight uint64
	}{
		{
			name: "regular_containers_sum_to_the_reference_pair",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "300m"),
					containerWithCPURequest("sidecar", "200m"),
				},
			},
			wantWeight: 20, // sums to 500m, the measured reference pair
		},
		{
			name: "init_container_larger_than_regular_sum_wins",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "100m"),
				},
				InitContainers: []corev1.Container{
					containerWithCPURequest("init", "1000m"),
				},
			},
			wantWeight: 39, // init container's 1000m dominates the 100m regular sum
		},
		{
			name: "init_container_smaller_than_regular_sum_is_ignored",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "500m"),
				},
				InitContainers: []corev1.Container{
					containerWithCPURequest("init", "50m"),
				},
			},
			wantWeight: 20, // regular sum 500m dominates the smaller init request
		},
		{
			name: "whole_cpu_requests",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "2"),
				},
			},
			wantWeight: 79, // 2 whole CPUs == 2000m
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RestoreWeight(tc.spec); got != tc.wantWeight {
				t.Fatalf("RestoreWeight() = %d, want %d", got, tc.wantWeight)
			}
		})
	}
}

// TestWeightTableNativeSidecar covers native sidecars (init containers with
// RestartPolicy Always): unlike a regular init container, a sidecar runs
// concurrently with the pod's regular containers for the pod's whole
// lifetime, so its request sums in rather than being maxed away.
func TestWeightTableNativeSidecar(t *testing.T) {
	tests := []struct {
		name       string
		spec       corev1.PodSpec
		wantWeight uint64
	}{
		{
			name: "sidecar_alone_sums_with_main_container",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "100m"),
				},
				InitContainers: []corev1.Container{
					nativeSidecar("sidecar", "200m"),
				},
			},
			wantWeight: 12, // 100m main + 200m sidecar sums to 300m, not max(100,200)=200m
		},
		{
			name: "multiple_native_sidecars_sum_together",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", ""),
				},
				InitContainers: []corev1.Container{
					nativeSidecar("sidecar-a", "80m"),
					nativeSidecar("sidecar-b", "120m"),
				},
			},
			wantWeight: 8, // both sidecars sum: 0m main + 80m + 120m = 200m
		},
		{
			name: "sidecar_plus_regular_init_where_running_total_dominates",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "300m"),
				},
				InitContainers: []corev1.Container{
					nativeSidecar("sidecar", "200m"),
					containerWithCPURequest("init", "50m"),
				},
			},
			// Running total (app 300m + sidecar 200m = 500m) beats the
			// regular init's own branch (its 50m + accumulated sidecar
			// 200m = 250m), since 250m < the sum of regular.
			wantWeight: 20,
		},
		{
			name: "sidecar_plus_regular_init_where_init_branch_dominates",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{
					containerWithCPURequest("app", "100m"),
				},
				InitContainers: []corev1.Container{
					nativeSidecar("sidecar", "50m"),
					containerWithCPURequest("init", "800m"),
				},
			},
			// The regular init's own branch (its 800m + accumulated
			// sidecar 50m = 850m) beats the running total (app 100m +
			// sidecar 50m = 150m), since 850m > the sum of regular.
			wantWeight: 34,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RestoreWeight(tc.spec); got != tc.wantWeight {
				t.Fatalf("RestoreWeight() = %d, want %d", got, tc.wantWeight)
			}
		})
	}
}
