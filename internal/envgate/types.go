package envgate

import "github.com/azalio/cpu-idle-operator/internal/cgroup"

// UnameFunc returns the running kernel's release string, in the same format
// as the third field of uname -r. It is injectable so callers and tests can
// pin the environment decision.
type UnameFunc func() (string, error)

// Result is Check's environment decision.
type Result struct {
	// Ready is true only when every environment check passed.
	Ready bool
	// Reason is always set, including on success.
	Reason Reason
	// Driver is empty when driver detection did not reach a conclusion.
	Driver cgroup.Driver
	// Experimental is true for the implemented but not stand-verified
	// cgroupfs driver.
	Experimental bool
}
