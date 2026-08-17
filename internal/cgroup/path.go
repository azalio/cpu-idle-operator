package cgroup

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/opencontainers/cgroups/systemd"
)

// PodCgroupPath returns the absolute path to a pod's cgroup v2 directory.
// It is computed purely from root, driver, qos and podUID: no /proc, no CRI
// socket, no container runtime call. root is the mounted cgroup v2
// hierarchy in production, but callers must always pass it explicitly so
// tests never touch the real filesystem.
func PodCgroupPath(root string, driver Driver, qos QoSClass, podUID string) (string, error) {
	if !driver.Valid() {
		return "", fmt.Errorf("cgroup: unknown driver %q", driver)
	}
	if !qos.Valid() {
		return "", fmt.Errorf("cgroup: unknown QoS class %q", qos)
	}
	if podUID == "" {
		return "", errors.New("cgroup: empty pod UID")
	}

	switch driver {
	case DriverSystemd:
		return systemdPodCgroupPath(root, qos, podUID)
	case DriverCgroupfs:
		return cgroupfsPodCgroupPath(root, qos, podUID), nil
	default:
		// Unreachable: driver.Valid() already rejected anything else.
		return "", fmt.Errorf("cgroup: unknown driver %q", driver)
	}
}

// systemdPodCgroupPath builds the pod slice name the same way kubelet's
// ToSystemd() conversion does, then expands it into the nested slice path
// with systemd's own algorithm so intermediate .slice components match
// exactly. Guaranteed pods have no QoS level in the path.
func systemdPodCgroupPath(root string, qos QoSClass, podUID string) (string, error) {
	// Intent: kubelet escapes UID dashes to underscores before handing the
	// name to systemd, which treats "-" as a slice-nesting separator.
	escapedUID := strings.ReplaceAll(podUID, "-", "_")
	podName := "pod" + escapedUID

	var sliceName string
	if qos == QoSGuaranteed {
		sliceName = "kubepods-" + podName + ".slice"
	} else {
		sliceName = "kubepods-" + string(qos) + "-" + podName + ".slice"
	}

	expanded, err := systemd.ExpandSlice(sliceName)
	if err != nil {
		return "", fmt.Errorf("cgroup: expand slice %q: %w", sliceName, err)
	}
	return path.Join(root, expanded), nil
}

// cgroupfsPodCgroupPath builds the flat cgroupfs path. UID dashes are kept
// as-is (unlike the systemd driver); Guaranteed pods have no QoS level
// directory.
func cgroupfsPodCgroupPath(root string, qos QoSClass, podUID string) string {
	podDir := "pod" + podUID
	if qos == QoSGuaranteed {
		return path.Join(root, "kubepods", podDir)
	}
	return path.Join(root, "kubepods", string(qos), podDir)
}
