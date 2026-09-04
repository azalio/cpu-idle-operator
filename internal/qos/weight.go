package qos

import (
	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"
)

// minShares is the floor kubelet applies before converting cgroup v1-style
// CPU shares into a cgroup v2 cpu.weight value: a pod with no CPU request
// still gets the minimum, never zero.
const (
	minShares = 2
	maxShares = 262144
)

// weightNumerator and weightDenominator are kubelet's own constants for
// mapping the [2,262144] shares range onto the [1,10000] cpu.weight range.
// The denominator is deliberately 262142, not 262142-2's naive-looking
// twin 262140: on the measured reference pair (requests.cpu=500m ->
// weight=20) both denominators produce the same result because of integer
// division, so that pair alone cannot catch a wrong denominator. 262140
// slipped through the first version of the stand script this exact way and
// was only caught on review — see resolutions T-005.md and T-006.md, and
// hack/stand-probe.sh's expected_weight, which carries the same warning.
const (
	weightNumerator   = 9999
	weightDenominator = 262142
)

// RestoreWeight computes the cpu.weight kubelet would assign to spec's pod
// cgroup, using kubelet's own shares-to-weight formula:
//
//	shares = max(2, milliCPU*1024/1000)
//	weight = 1 + ((shares-2)*9999)/262142
//
// This is the value the agent must write back after clearing cpu.idle: the
// kernel resets cpu.weight to its own default (100) rather than kubelet's
// request-derived value when idle is cleared, so nothing else restores it
// (resolution T-006.md).
//
// The source of truth is always the live spec passed in, never a value
// cached when the pod entered idle: if requests.cpu changed while the pod
// was idle, restoring must honor the new request, not the stale one.
func RestoreWeight(spec corev1.PodSpec) uint64 {
	// ResourceConfigForPod ignores requests and installs MinShares for a
	// BestEffort pod. This matters for Kubernetes 1.36's explicitly empty
	// spec.resources override: container requests can exist even though the
	// kubelet still classifies the pod as BestEffort.
	if ClassOf(spec) == BestEffort {
		return 1
	}
	requests := resourcehelper.PodRequests(&corev1.Pod{Spec: spec}, resourcehelper.PodResourcesOptions{})
	milliCPU := requests.Cpu().MilliValue()
	shares := milliCPU * 1024 / 1000
	if shares < minShares {
		shares = minShares
	}
	if shares > maxShares {
		shares = maxShares
	}
	return uint64(1 + ((shares-minShares)*weightNumerator)/weightDenominator)
}
