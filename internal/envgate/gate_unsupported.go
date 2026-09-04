//go:build !linux

package envgate

import (
	"os"
	"path/filepath"
)

// Check fails closed on non-Linux hosts. Keeping the public seam available
// lets the platform-independent lifecycle tests compile and run there; the
// production DaemonSet remains Linux-only.
func Check(root string, _ string, _ UnameFunc) (Result, error) {
	if isDirectory(filepath.Join(root, "cpu")) {
		return Result{Ready: false, Reason: ReasonCgroupV1}, nil
	}
	if isDirectory(filepath.Join(root, "unified")) {
		return Result{Ready: false, Reason: ReasonCgroupHybrid}, nil
	}
	return Result{Ready: false, Reason: ReasonDriverUnknown}, nil
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
