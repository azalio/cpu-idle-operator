// Package envgate decides, from static and injectable filesystem/kernel
// facts, whether this node's cgroup environment can host the CPU idle
// tier: cgroup v2 unified only, a recognized kubepods cgroup driver, and a
// kernel new enough for cpu.idle on cgroup entities (>= 5.15). It never
// writes anything under the cgroup root it inspects and never terminates
// the process — it only returns a decision for the caller to act on.
// Callers must not perform any cgroup write when Result.Ready is false.
package envgate

// Reason names why Check reached its Ready decision. It is always set to a
// concrete value, including on success (ReasonOK), so logs and metrics
// never have to special-case an empty string.
type Reason string

const (
	// ReasonOK means every check passed: cgroup v2 unified, a recognized
	// kubepods driver, a kernel new enough for cpu.idle on cgroup entities,
	// and all required CPU control files present.
	ReasonOK Reason = "ok"
	// ReasonCgroupV1 means root is a cgroup v1 hierarchy: a "cpu"
	// controller directory exists directly under root. cpu.idle does not
	// exist for cgroup entities in v1, so there is nothing to write.
	ReasonCgroupV1 Reason = "cgroup_v1"
	// ReasonCgroupHybrid means root is a v1/v2 hybrid mount: a "unified"
	// directory exists under root. The cpu controller stays on v1 in
	// hybrid mode, so cpu.idle is still unavailable there.
	ReasonCgroupHybrid Reason = "cgroup_hybrid"
	// ReasonKernelTooOld means the running kernel predates 5.15, the
	// version cpu.idle for cgroup entities landed upstream.
	ReasonKernelTooOld Reason = "kernel_too_old"
	// ReasonKernelUnknown means uname failed or returned a release string
	// whose major/minor version could not be parsed. The agent cannot prove
	// cpu.idle support, so it stays alive but fails the gate closed.
	ReasonKernelUnknown Reason = "kernel_unknown"
	// ReasonKubepodsMissing means root is a confirmed clean cgroup v2
	// mount, but neither a systemd nor a cgroupfs kubepods directory
	// exists yet. This is the expected state on a node kubelet has not
	// scheduled any pods on yet, and is reported separately from
	// ReasonDriverUnknown so operators can tell "nothing here yet" apart
	// from "something is wrong here".
	ReasonKubepodsMissing Reason = "kubepods_missing"
	// ReasonDriverUnknown means root's cgroup layout could not be
	// classified: either root is not a confirmed clean v2 mount
	// (cgroup.controllers plus a cgroup2fs mount) and neither the v1 nor
	// the hybrid marker is present either, or both the systemd and the
	// cgroupfs kubepods paths exist at once, which no real driver
	// produces.
	ReasonDriverUnknown Reason = "driver_unknown"
	// ReasonRequiredKnobMissing means the kubepods cgroup exists but at
	// least one control file the agent must read or write is unavailable.
	ReasonRequiredKnobMissing Reason = "required_knob_missing"
)
