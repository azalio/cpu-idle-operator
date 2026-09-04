package apply

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/azalio/cpu-idle-operator/internal/cgroup"
)

// cpuMaxUnbounded is cpu.max's first field when no quota is configured.
const cpuMaxUnbounded = "max"

const (
	minCPUQuotaPeriod = 1_000
	maxCPUQuotaPeriod = 1_000_000
	idleCPUWeight     = 0
	minCPUWeight      = 1
	maxCPUWeight      = 10_000
)

// Snapshot is a pod cgroup's actual cpu.idle, cpu.weight, cpu.max and
// cpu.max.burst values. Applier reads a Snapshot exactly once, at the start
// of each Apply call: re-reading any of these files between planned writes
// would let a value the plan was built against change out from under it,
// opening a race window and making the plan BuildPlan returned
// non-reproducible.
type Snapshot struct {
	// IdleActive is cpu.idle's current value: true when it reads "1".
	IdleActive bool
	// Weight is cpu.weight's current value. Mainline kernels report 0 while
	// cpu.idle is active; some downstream kernels expose the same effective
	// minimum as 1. Otherwise the writable range is [1, 10000].
	Weight uint64
	// HasQuota is true when cpu.max carries a numeric quota rather than
	// "max" (unbounded — no quota configured for this pod cgroup).
	HasQuota bool
	// Quota is cpu.max's quota component, valid only when HasQuota is
	// true.
	Quota uint64
	// Burst is cpu.max.burst's current value.
	Burst uint64
}

// ReadSnapshot reads dir's cpu.idle, cpu.weight, cpu.max and cpu.max.burst
// knob files exactly once. If dir does not exist, the first cgroup.ReadKnob
// call returns cgroup.ErrCgroupGone and ReadSnapshot returns that same error
// unwrapped; the lifecycle-aware caller decides whether this is an expected
// creation/deletion race or a Running pod that needs retrying.
func ReadSnapshot(dir string) (Snapshot, error) {
	idleRaw, err := cgroup.ReadKnob(dir, KnobCPUIdle)
	if err != nil {
		return Snapshot{}, err
	}
	weightRaw, err := cgroup.ReadKnob(dir, KnobCPUWeight)
	if err != nil {
		return Snapshot{}, err
	}
	maxRaw, err := cgroup.ReadKnob(dir, KnobCPUMax)
	if err != nil {
		return Snapshot{}, err
	}
	burstRaw, err := cgroup.ReadKnob(dir, KnobCPUMaxBurst)
	if err != nil {
		return Snapshot{}, err
	}
	if idleRaw != "0" && idleRaw != "1" {
		return Snapshot{}, fmt.Errorf("apply: parse %s %q: expected 0 or 1", KnobCPUIdle, idleRaw)
	}

	weight, err := strconv.ParseUint(weightRaw, 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("apply: parse %s %q: %w", KnobCPUWeight, weightRaw, err)
	}
	validIdleWeight := idleRaw == "1" && weight == idleCPUWeight
	if !validIdleWeight && (weight < minCPUWeight || weight > maxCPUWeight) {
		return Snapshot{}, fmt.Errorf("apply: parse %s %q: expected 0 for an idle cgroup or a value in [%d, %d]", KnobCPUWeight, weightRaw, minCPUWeight, maxCPUWeight)
	}
	burst, err := strconv.ParseUint(burstRaw, 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("apply: parse %s %q: %w", KnobCPUMaxBurst, burstRaw, err)
	}
	hasQuota, quota, err := parseCPUMaxQuota(maxRaw)
	if err != nil {
		return Snapshot{}, fmt.Errorf("apply: parse %s %q: %w", KnobCPUMax, maxRaw, err)
	}

	return Snapshot{
		IdleActive: idleRaw == "1",
		Weight:     weight,
		HasQuota:   hasQuota,
		Quota:      quota,
		Burst:      burst,
	}, nil
}

// parseCPUMaxQuota parses cpu.max's first field: "<quota> <period>" when a
// quota is configured, or "max <period>" when it is not. Treating "max" as
// a number here — instead of recognizing it as the unbounded sentinel — is
// exactly the bug this function exists to prevent: the risk this subtask
// was named for is an agent that parses "max" as a quota and computes an
// absurd burst value from it.
func parseCPUMaxQuota(raw string) (hasQuota bool, quota uint64, err error) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return false, 0, fmt.Errorf("expected '<quota|max> <period>', got %q", raw)
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period < minCPUQuotaPeriod || period > maxCPUQuotaPeriod {
		return false, 0, fmt.Errorf("period field %q is outside [%d, %d]", fields[1], minCPUQuotaPeriod, maxCPUQuotaPeriod)
	}
	if fields[0] == cpuMaxUnbounded {
		return false, 0, nil
	}
	quota, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return false, 0, fmt.Errorf("quota field %q: %w", fields[0], err)
	}
	if quota < minCPUQuotaPeriod {
		return false, 0, fmt.Errorf("quota field %q is below %d", fields[0], minCPUQuotaPeriod)
	}
	return true, quota, nil
}
