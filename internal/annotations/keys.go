// Package annotations is the single source of truth for the pod annotation
// keys that cpu-idle-operator reacts to. No other package in this tree may
// spell out the "cpu.azalio.net/" literal directly.
package annotations

// domainPrefix is the only place the annotation domain literal is written.
const domainPrefix = "cpu.azalio.net/"

const (
	// TierKey selects the CPU tier for a pod, e.g. TierValueIdle.
	TierKey = domainPrefix + "tier"
	// TierValueIdle is the TierKey value that requests cpu.idle=1 on the pod cgroup.
	TierValueIdle = "idle"
	// BurstKey requests cpu.max.burst equal to the pod's CPU quota.
	BurstKey = domainPrefix + "burst"
	// GuardStateKey is an internal ownership marker. Before the node guard
	// suppresses a pod, it records the exact knob/value to restore here so
	// recovery survives enabled process restarts and explicit cleanup.
	GuardStateKey = domainPrefix + "guard-state"
)
