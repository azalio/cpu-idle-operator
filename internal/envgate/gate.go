//go:build linux

package envgate

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

// minKernelMajor and minKernelMinor are the lowest kernel version this
// operator supports: cpu.idle for cgroup entities landed upstream in 5.15.
const (
	minKernelMajor = 5
	minKernelMinor = 15
)

// UnameFunc returns the running kernel's release string, in the same
// format as the third field of `uname -r` (e.g. "6.17.0-061700-generic").
// It is a Check parameter rather than a direct syscall so tests can pin the
// kernel version without depending on the host the tests run on.
type UnameFunc func() (string, error)

// Result is Check's environment decision.
type Result struct {
	// Ready is true only when every check passed: cgroup v2 unified, a
	// recognized kubepods driver, and kernel >= 5.15.
	Ready bool
	// Reason explains the decision. It is always set, even when Ready is
	// true (ReasonOK).
	Reason Reason
	// Driver is the detected cgroup driver. It is the zero value when
	// detection never reached a conclusion (ReasonCgroupV1,
	// ReasonCgroupHybrid, ReasonKubepodsMissing, ReasonDriverUnknown).
	Driver cgroup.Driver
	// Experimental is true when Driver is cgroup.DriverCgroupfs: this path
	// is implemented but unverified on a live stand (see
	// hack/stand-probe.sh), unlike DriverSystemd which is stand-verified.
	Experimental bool
}

// warnLogger receives Check's single experimental-driver warning. It is a
// package variable rather than a Check parameter because the Check
// signature is fixed by the subtask contract; tests swap this var to
// capture the emitted record instead of asserting against a real handler's
// output stream.
var warnLogger = slog.Default()

// statfsType reports the filesystem magic number mounted at path, matching
// the Type field of unix.Statfs_t (compare against unix.CGROUP2_SUPER_MAGIC
// for a cgroup v2 mount). It is a package variable, not a Check parameter,
// so tests can simulate a cgroup2fs mount: t.TempDir() is always a regular
// filesystem, and a real cgroup2 mount cannot be created inside a unit
// test.
var statfsType = func(path string) (int64, error) {
	var buf unix.Statfs_t
	if err := unix.Statfs(path, &buf); err != nil {
		return 0, err
	}
	return int64(buf.Type), nil
}

// Check decides whether root is an environment the CPU idle tier can run
// on. It only reads the filesystem (Stat-level existence checks and a
// statfs call) and calls uname; it never writes under root and never calls
// os.Exit. Callers must not perform any cgroup write when the returned
// Result.Ready is false — Check itself performs zero writes regardless of
// its decision. kubepodsName is the top-level kubepods slice/directory name
// this check looks for under root (cgroup.DefaultKubepodsName for a stock
// kubelet; a kubelet-root-prefixed name, e.g. "kubelet-kubepods" on kind,
// for a kubelet started with a non-default --cgroup-root).
//
// The non-nil error return is reserved for uname itself failing or
// returning a release string Check cannot parse at all; every filesystem
// outcome Check can classify is reported through Result.Reason instead, so
// a caller can always log and expose a decision without a crash loop.
func Check(root, kubepodsName string, uname UnameFunc) (Result, error) {
	if reason := cgroupVersionReason(root); reason != ReasonOK {
		return Result{Ready: false, Reason: reason}, nil
	}

	driver, reason := detectDriver(root, kubepodsName)
	if reason != ReasonOK {
		return Result{Ready: false, Reason: reason}, nil
	}

	release, err := uname()
	if err != nil {
		return Result{}, fmt.Errorf("envgate: uname: %w", err)
	}
	newEnough, err := kernelAtLeast(release, minKernelMajor, minKernelMinor)
	if err != nil {
		return Result{}, fmt.Errorf("envgate: parse kernel release %q: %w", release, err)
	}

	result := Result{Driver: driver}
	if driver == cgroup.DriverCgroupfs {
		result.Experimental = true
		// Intent: cgroupfs is implemented but has no stand run behind it
		// (see hack/stand-probe.sh preflight, which refuses this driver
		// outright); operators need this called out once per Check.
		warnLogger.Warn("cgroup driver detected as cgroupfs: this path is experimental and unverified on a live stand",
			"driver", string(driver))
	}

	if !newEnough {
		result.Ready = false
		result.Reason = ReasonKernelTooOld
		return result, nil
	}

	result.Ready = true
	result.Reason = ReasonOK
	return result, nil
}

// cgroupVersionReason classifies root's cgroup mount. It returns ReasonOK
// only when root is a confirmed clean cgroup v2 unified hierarchy: the v1
// and hybrid markers are checked first (and independently of the v2
// confirmation) because a real v1 or hybrid root would otherwise just fail
// the v2 confirmation with no specific reason to report.
func cgroupVersionReason(root string) Reason {
	if dirExists(filepath.Join(root, "cpu")) {
		return ReasonCgroupV1
	}
	if dirExists(filepath.Join(root, "unified")) {
		return ReasonCgroupHybrid
	}
	if !fileExists(filepath.Join(root, "cgroup.controllers")) {
		return ReasonDriverUnknown
	}
	fsType, err := statfsType(root)
	if err != nil || fsType != unix.CGROUP2_SUPER_MAGIC {
		return ReasonDriverUnknown
	}
	return ReasonOK
}

// detectDriver classifies the kubepods cgroup driver from the v2 paths
// kubelet creates under root, using kubepodsName as the top-level
// slice/directory name (see Check's doc comment). It must only run after
// cgroupVersionReason has already confirmed root is clean v2: the v1-era
// heuristic of looking for <root>/cpu/kubepods.slice does not apply here and
// must not be reintroduced (see resolution T-002 review note: a clean v2
// node never has a cpu/ directory at all).
func detectDriver(root, kubepodsName string) (cgroup.Driver, Reason) {
	hasSystemd := dirExists(filepath.Join(root, kubepodsName+".slice"))
	hasCgroupfs := dirExists(filepath.Join(root, kubepodsName))

	switch {
	case hasSystemd && !hasCgroupfs:
		return cgroup.DriverSystemd, ReasonOK
	case hasCgroupfs && !hasSystemd:
		return cgroup.DriverCgroupfs, ReasonOK
	case !hasSystemd && !hasCgroupfs:
		return "", ReasonKubepodsMissing
	default:
		// Both paths exist at once: no real kubelet driver produces this,
		// so it is reported distinctly from the empty-node case above.
		return "", ReasonDriverUnknown
	}
}

// kernelAtLeast reports whether release is >= minMajor.minMinor, comparing
// only the major.minor components. release may carry a distro suffix after
// the version proper (e.g. "6.17.0-061700-generic" on the reference stand);
// everything from the first "-" onward is ignored.
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

// dirExists reports whether path exists and is a directory. Any stat error
// (including "does not exist") is treated as absent: Check must stay
// side-effect-free and fail closed rather than surface a raw stat error for
// a condition its Reason vocabulary already covers.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// fileExists reports whether path exists and is a regular (non-directory)
// entry. See dirExists for why stat errors collapse to false.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
