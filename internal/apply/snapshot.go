package apply

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

// cpuMaxUnbounded is cpu.max's first field when no quota is configured.
const cpuMaxUnbounded = "max"

// Snapshot is a pod cgroup's actual cpu.idle, cpu.weight, cpu.max and
// cpu.max.burst values. Applier reads a Snapshot exactly once, at the start
// of each Apply call: re-reading any of these files between planned writes
// would let a value the plan was built against change out from under it,
// opening a race window and making the plan BuildPlan returned
// non-reproducible.
type Snapshot struct {
	// IdleActive is cpu.idle's current value: true when it reads "1".
	IdleActive bool
	// Weight is cpu.weight's current value.
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
// knob files exactly once. If dir no longer exists — the pod was deleted
// between the informer handing it to the caller and this read — the first
// cgroup.ReadKnob call returns cgroup.ErrCgroupGone and ReadSnapshot
// returns that same error, unwrapped through, for the caller to treat as
// "nothing to do" rather than a failure.
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

	weight, err := strconv.ParseUint(weightRaw, 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("apply: parse %s %q: %w", KnobCPUWeight, weightRaw, err)
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
	if len(fields) == 0 {
		return false, 0, fmt.Errorf("empty %s content", KnobCPUMax)
	}
	if fields[0] == cpuMaxUnbounded {
		return false, 0, nil
	}
	quota, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return false, 0, fmt.Errorf("quota field %q: %w", fields[0], err)
	}
	return true, quota, nil
}
