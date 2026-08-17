// Package cgroup computes pod cgroup v2 paths and reads/writes individual
// cgroup knob files. It never touches /proc, a CRI socket, or a container
// runtime: every exported function takes the cgroup root as a parameter and
// derives the rest purely from pod metadata already known to the caller.
package cgroup

// Driver identifies which cgroup driver kubelet uses to name pod cgroups.
type Driver string

const (
	// DriverSystemd names pod cgroups as nested systemd slices, e.g.
	// kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice.
	DriverSystemd Driver = "systemd"
	// DriverCgroupfs names pod cgroups as plain nested directories, e.g.
	// kubepods/burstable/pod<uid>.
	DriverCgroupfs Driver = "cgroupfs"
)

// Valid reports whether d is a recognized cgroup driver.
func (d Driver) Valid() bool {
	switch d {
	case DriverSystemd, DriverCgroupfs:
		return true
	default:
		return false
	}
}

func (d Driver) String() string { return string(d) }

// QoSClass is a Kubernetes pod QoS class. It is redeclared here (rather than
// importing k8s.io/api/core/v1) so this package stays free of API-server
// and runtime dependencies.
type QoSClass string

const (
	// QoSGuaranteed pods get no QoS level in their cgroup path.
	QoSGuaranteed QoSClass = "guaranteed"
	// QoSBurstable pods are nested under a burstable QoS slice/directory.
	QoSBurstable QoSClass = "burstable"
	// QoSBestEffort pods are nested under a besteffort QoS slice/directory.
	QoSBestEffort QoSClass = "besteffort"
)

// Valid reports whether q is a recognized QoS class.
func (q QoSClass) Valid() bool {
	switch q {
	case QoSGuaranteed, QoSBurstable, QoSBestEffort:
		return true
	default:
		return false
	}
}

func (q QoSClass) String() string { return string(q) }
