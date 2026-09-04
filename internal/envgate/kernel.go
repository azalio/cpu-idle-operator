package envgate

import (
	"fmt"
	"strconv"
	"strings"
)

// minKernelMajor and minKernelMinor are the lowest kernel version this
// operator supports: cpu.idle for cgroup entities landed upstream in 5.15.
const (
	minKernelMajor = 5
	minKernelMinor = 15
)

// kernelReason classifies the running kernel without leaking uname or parsing
// failures as process-fatal errors. It is platform-independent so this policy
// remains unit-testable on developer machines even though the filesystem half
// of the environment gate is Linux-only.
func kernelReason(uname UnameFunc) Reason {
	if uname == nil {
		return ReasonKernelUnknown
	}
	release, err := uname()
	if err != nil {
		return ReasonKernelUnknown
	}
	newEnough, err := kernelAtLeast(release, minKernelMajor, minKernelMinor)
	if err != nil {
		return ReasonKernelUnknown
	}
	if !newEnough {
		return ReasonKernelTooOld
	}
	return ReasonOK
}

// kernelAtLeast reports whether release is >= minMajor.minMinor, comparing
// only the major.minor components. release may carry a distro suffix after
// the version proper (e.g. "6.17.0-061700-generic"); everything from the
// first "-" onward is ignored.
func kernelAtLeast(release string, minMajor, minMinor int) (bool, error) {
	base, _, _ := strings.Cut(release, "-")
	parts := strings.SplitN(base, ".", 3)
	if len(parts) < 2 {
		return false, fmt.Errorf("expected at least major.minor in %q", release)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false, fmt.Errorf("major version %q: %w", parts[0], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, fmt.Errorf("minor version %q: %w", parts[1], err)
	}

	if major != minMajor {
		return major > minMajor, nil
	}
	return minor >= minMinor, nil
}
