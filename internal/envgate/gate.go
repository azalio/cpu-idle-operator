//go:build linux

package envgate

import (
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/azalio/cpu-idle-operator/internal/cgroup"
)

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
// Every environment outcome, including uname failure or an unparseable
// release, is reported through Result.Reason so a caller can always expose a
// fail-closed decision without a crash loop. The error result remains in the
// API for callers that inject a future check with a genuinely internal error.
func Check(root, kubepodsName string, uname UnameFunc) (Result, error) {
	if reason := cgroupVersionReason(root); reason != ReasonOK {
		return Result{Ready: false, Reason: reason}, nil
	}

	driver, reason := detectDriver(root, kubepodsName)
	if reason != ReasonOK {
		return Result{Ready: false, Reason: reason}, nil
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
	if reason := kernelReason(uname); reason != ReasonOK {
		result.Ready = false
		result.Reason = reason
		return result, nil
	}
	if !requiredCPUKnobsPresent(root, kubepodsName, driver) {
		result.Ready = false
		result.Reason = ReasonRequiredKnobMissing
		return result, nil
	}

	result.Ready = true
	result.Reason = ReasonOK
	return result, nil
}

var requiredCPUKnobs = [...]string{"cpu.idle", "cpu.weight", "cpu.max", "cpu.max.burst"}

func requiredCPUKnobsPresent(root, kubepodsName string, driver cgroup.Driver) bool {
	dir := filepath.Join(root, kubepodsName)
	if driver == cgroup.DriverSystemd {
		dir += ".slice"
	}
	for _, knob := range requiredCPUKnobs {
		if !fileExists(filepath.Join(dir, knob)) {
			return false
		}
	}
	return true
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
