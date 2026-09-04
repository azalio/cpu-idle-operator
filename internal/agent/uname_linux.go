//go:build linux

package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// realUname reports this host's running Linux kernel release.
func realUname() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", fmt.Errorf("agent: uname: %w", err)
	}
	return unix.ByteSliceToString(uts.Release[:]), nil
}
