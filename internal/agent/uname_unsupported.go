//go:build !linux

package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func realUname() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", fmt.Errorf("agent: uname: %w", err)
	}
	release := make([]byte, 0, len(uts.Release))
	for _, value := range uts.Release {
		if value == 0 {
			break
		}
		release = append(release, byte(value))
	}
	return string(release), nil
}
