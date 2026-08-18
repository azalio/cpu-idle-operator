package qos

import (
	corev1 "k8s.io/api/core/v1"
)

// minShares is the floor kubelet applies before converting cgroup v1-style
// CPU shares into a cgroup v2 cpu.weight value: a pod with no CPU request
// still gets the minimum, never zero.
const minShares = 2

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
	milliCPU := effectiveMilliCPURequest(spec)
	shares := milliCPU * 1024 / 1000
	if shares < minShares {
		shares = minShares
	}
	return 1 + ((shares-minShares)*weightNumerator)/weightDenominator
}

// effectiveMilliCPURequest mirrors kubelet's own PodRequests computation
// (k8s.io/kubectl/pkg/util/resource.podRequests): regular containers all run
// concurrently, so their requests sum. A native sidecar — an init container
// with RestartPolicy Always — also runs for the pod's entire lifetime
// alongside the regular containers, so its request sums in too and
// accumulates in a running sidecar total. A regular (non-sidecar) init
// container still runs sequentially before the others, so it is only
// compared via max — but against its own request plus whatever sidecar
// total has accumulated by that point in the init list, since sidecars that
// started earlier are already running concurrently with it. The result is
// max(regularTotal, largest init-branch value), where regularTotal already
// includes every sidecar's request.
func effectiveMilliCPURequest(spec corev1.PodSpec) uint64 {
	var regularTotal uint64
	for _, container := range spec.Containers {
		regularTotal += containerMilliCPU(container)
	}

	var sidecarTotal uint64
	var initMax uint64
	for _, container := range spec.InitContainers {
		milli := containerMilliCPU(container)
		var branch uint64
		if isNativeSidecar(container) {
			sidecarTotal += milli
			regularTotal += milli
			branch = sidecarTotal
		} else {
			branch = milli + sidecarTotal
		}
		if branch > initMax {
			initMax = branch
		}
	}

	if initMax > regularTotal {
		return initMax
	}
	return regularTotal
}

// isNativeSidecar reports whether container is a native sidecar — an init
// container with RestartPolicy Always, which kubelet keeps running
// concurrently with the pod's regular containers rather than running it to
// completion before them.
func isNativeSidecar(container corev1.Container) bool {
	return container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// containerMilliCPU returns container's requests.cpu in milli-CPU units, or
// 0 when the container has no CPU request (or, defensively, a negative one
// — the API server rejects negative resources, but this function stays a
// pure projection of whatever spec it is handed).
func containerMilliCPU(container corev1.Container) uint64 {
	quantity, ok := container.Resources.Requests[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	milli := quantity.MilliValue()
	if milli < 0 {
		return 0
	}
	return uint64(milli)
}
