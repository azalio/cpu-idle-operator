package cgroup

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/opencontainers/cgroups/systemd"
)

// DefaultKubepodsName is the top-level kubepods cgroup name a stock kubelet
// creates: kubelet's own --cgroup-root defaults to the real cgroupfs root,
// so the slice/directory kubelet names its pod hierarchy under is plain
// "kubepods". A kubelet started with a non-default --cgroup-root (e.g.
// kind, which uses "/kubelet") instead prefixes every kubepods
// slice/directory name with its own root's basename — see PodCgroupPath's
// doc comment for the systemd-driver mechanics that follow from that.
const DefaultKubepodsName = "kubepods"

// PodCgroupPath returns the absolute path to a pod's cgroup v2 directory.
// It is computed purely from root, kubepodsName, driver, qos and podUID: no
// /proc, no CRI socket, no container runtime call. root is the mounted
// cgroup v2 hierarchy in production, but callers must always pass it
// explicitly so tests never touch the real filesystem. kubepodsName is the
// top-level kubepods slice/directory name kubelet actually uses —
// DefaultKubepodsName for a stock kubelet, or a kubelet-root-prefixed name
// (e.g. "kubelet-kubepods" on kind) for a kubelet started with a
// non-default --cgroup-root.
func PodCgroupPath(root, kubepodsName string, driver Driver, qos QoSClass, podUID string) (string, error) {
	if !driver.Valid() {
		return "", fmt.Errorf("cgroup: unknown driver %q", driver)
	}
	if !qos.Valid() {
		return "", fmt.Errorf("cgroup: unknown QoS class %q", qos)
	}
	if podUID == "" {
		return "", errors.New("cgroup: empty pod UID")
	}
	if kubepodsName == "" {
		return "", errors.New("cgroup: empty kubepods name")
	}

	switch driver {
	case DriverSystemd:
		return systemdPodCgroupPath(root, kubepodsName, qos, podUID)
	case DriverCgroupfs:
		return cgroupfsPodCgroupPath(root, kubepodsName, qos, podUID), nil
	default:
		// Unreachable: driver.Valid() already rejected anything else.
		return "", fmt.Errorf("cgroup: unknown driver %q", driver)
	}
}

// systemdPodCgroupPath builds the pod slice name the same way kubelet's
// ToSystemd() conversion does, then expands it into the nested slice path
// with systemd's own algorithm so intermediate .slice components match
// exactly. Guaranteed pods have no QoS level in the path.
//
// kubepodsName may itself carry systemd's dash-nesting (e.g. kind's
// "kubelet-kubepods", nested under a "kubelet.slice" root kubelet creates
// for its own non-default --cgroup-root): systemd.ExpandSlice re-derives
// that outer nesting from scratch starting at "/", but root already names
// the directory that outer nesting lives under, so the shared leading
// fragment is stripped once before joining onto root — otherwise the outer
// slice component would appear twice in the returned path (measured on
// kind, with root set to the mounted cgroup v2 hierarchy's "kubelet.slice"
// child and kubepodsName "kubelet-kubepods").
func systemdPodCgroupPath(root, kubepodsName string, qos QoSClass, podUID string) (string, error) {
	// Intent: kubelet escapes UID dashes to underscores before handing the
	// name to systemd, which treats "-" as a slice-nesting separator.
	escapedUID := strings.ReplaceAll(podUID, "-", "_")
	podName := "pod" + escapedUID

	var sliceName string
	if qos == QoSGuaranteed {
		sliceName = kubepodsName + "-" + podName + ".slice"
	} else {
		sliceName = kubepodsName + "-" + string(qos) + "-" + podName + ".slice"
	}

	expanded, err := systemd.ExpandSlice(sliceName)
	if err != nil {
		return "", fmt.Errorf("cgroup: expand slice %q: %w", sliceName, err)
	}

	kubepodsExpanded, err := systemd.ExpandSlice(kubepodsName + ".slice")
	if err != nil {
		return "", fmt.Errorf("cgroup: expand slice %q: %w", kubepodsName+".slice", err)
	}
	// outerPrefix is everything ExpandSlice nested kubepodsName's own slice
	// under -- "/" when kubepodsName has no dash of its own (the default
	// "kubepods" case: nothing to strip), or the dash-nested parent path
	// otherwise (e.g. "/kubelet.slice"). expanded always has kubepodsExpanded
	// as an exact prefix (both are built by the same progressive algorithm
	// over kubepodsName's own leading components), so kubepodsExpanded's own
	// parent is always a prefix of expanded too.
	outerPrefix := path.Dir(kubepodsExpanded)
	if outerPrefix != "/" {
		trimmed := strings.TrimPrefix(expanded, outerPrefix)
		if trimmed == expanded {
			// Unreachable given the construction above, but fails loudly
			// rather than silently double-nesting root if ExpandSlice's
			// algorithm ever changes underneath this package.
			return "", fmt.Errorf("cgroup: expanded slice %q does not share outer prefix %q", expanded, outerPrefix)
		}
		expanded = trimmed
	}

	return path.Join(root, expanded), nil
}

// cgroupfsPodCgroupPath builds the flat cgroupfs path. UID dashes are kept
// as-is (unlike the systemd driver); Guaranteed pods have no QoS level
// directory. Unlike the systemd driver, kubepodsName here is just one more
// literal directory component nested under root — cgroupfs has no
// dash-nesting to re-derive, so no double-root case exists on this branch.
func cgroupfsPodCgroupPath(root, kubepodsName string, qos QoSClass, podUID string) string {
	podDir := "pod" + podUID
	if qos == QoSGuaranteed {
		return path.Join(root, kubepodsName, podDir)
	}
	return path.Join(root, kubepodsName, string(qos), podDir)
}
