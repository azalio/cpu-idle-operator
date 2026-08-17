package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrCgroupGone indicates the pod cgroup directory disappeared between the
// moment the caller learned about the pod (e.g. from an informer cache) and
// the moment it tried to read or write a knob. The pod was deleted; this is
// an expected race, not a bug, and callers should treat it as "nothing to
// do" rather than retry or alert.
var ErrCgroupGone = errors.New("cgroup: pod cgroup is gone")

// ErrNotPodCgroup indicates dir does not point at an individual pod cgroup.
// WriteKnob refuses to write above pod level: the kubepods root, a
// QoS-level slice/directory shared by many pods, and a container scope are
// all rejected, so a caller cannot accidentally widen the blast radius of a
// single knob write to every pod under that level.
var ErrNotPodCgroup = errors.New("cgroup: target is not a pod cgroup")

// knobWriter is the subset of *os.File that WriteKnob needs. It exists so
// tests can inject a fake whose Close returns an error, proving that error
// surfaces and takes priority over a deferred Write error — the failure
// mode this package guards against, since the kernel can accept a
// cgroup-knob write() and only reject the value when the descriptor closes.
type knobWriter interface {
	Write(p []byte) (int, error)
	Close() error
}

// openKnobWriter opens a knob file for writing. It is a package variable
// (not a WriteKnob parameter) because the exported signature is fixed by
// the package contract; tests swap it to simulate a Close failure that a
// real filesystem cannot be coerced into reliably.
var openKnobWriter = func(path string) (knobWriter, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
}

// ReadKnob reads and trims the contents of the knob file dir/name.
func ReadKnob(dir, name string) (string, error) {
	p := filepath.Join(dir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrCgroupGone, p)
		}
		return "", fmt.Errorf("cgroup: read knob %s: %w", p, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteKnob writes value to the knob file dir/name. root is the caller's
// configured cgroup root (e.g. config.Config.CgroupRoot) — the same value
// passed to PodCgroupPath to compute dir in the first place. WriteKnob
// requires it explicitly, rather than inferring a root from dir itself, so
// the write-target guard cannot be fooled by a directory shaped like a
// second, fake pod cgroup nested inside a real one (see guardWriteTarget).
//
// Close errors take priority over a deferred Write error: the kernel can
// accept the write() syscall on a cgroup knob and only reject the value
// once the file descriptor is closed. A caller that only checks the Write
// return value would observe a false success where the kernel actually
// rejected the write (e.g. EINVAL on cpu.weight while cpu.idle=1). Both the
// Close and Write error paths preserve the underlying error chain, so
// callers can still do errors.Is(err, syscall.EINVAL) to distinguish a
// kernel-rejected value from any other failure.
func WriteKnob(root, dir, name, value string) error {
	if err := guardWriteTarget(dir, root); err != nil {
		return err
	}

	p := filepath.Join(dir, name)
	f, err := openKnobWriter(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrCgroupGone, p)
		}
		return fmt.Errorf("cgroup: open knob %s: %w", p, err)
	}

	_, writeErr := f.Write([]byte(value))
	closeErr := f.Close()
	if closeErr != nil {
		return fmt.Errorf("cgroup: close knob %s: %w", p, closeErr)
	}
	if writeErr != nil {
		return fmt.Errorf("cgroup: write knob %s: %w", p, writeErr)
	}
	return nil
}

// extractPodUID recovers the pod UID that PodCgroupPath would need to
// produce base as the final path component for driver/qos, or reports
// ok=false if base does not even have that shape. This is only a candidate
// generator: guardWriteTarget still recomputes the full path with
// PodCgroupPath against the caller-supplied root and requires an exact
// match, so a wrong or malformed uid here cannot cause a false accept — it
// just fails to reconstruct.
func extractPodUID(base string, driver Driver, qos QoSClass) (uid string, ok bool) {
	switch driver {
	case DriverCgroupfs:
		const prefix = "pod"
		if !strings.HasPrefix(base, prefix) {
			return "", false
		}
		return strings.TrimPrefix(base, prefix), true
	case DriverSystemd:
		prefix := "kubepods-"
		if qos != QoSGuaranteed {
			prefix += string(qos) + "-"
		}
		prefix += "pod"
		const suffix = ".slice"
		if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
			return "", false
		}
		return strings.TrimSuffix(strings.TrimPrefix(base, prefix), suffix), true
	default:
		return "", false
	}
}

// guardWriteTarget rejects any dir that is not the individual pod cgroup
// PodCgroupPath would compute under root. It is an allow-list, not a
// block-list: dir passes only if it is exactly PodCgroupPath(root, driver,
// qos, uid) for some valid driver/QoS/pod-UID combination. Everything else
// is rejected — the kubepods root, a QoS-level slice/directory, a container
// scope, an arbitrary directory, a non-pod subdirectory nested inside a
// real QoS slice, a relative path, a path that only resembles a pod cgroup
// before ".." resolves it elsewhere, and a directory shaped like a second,
// fake pod cgroup nested inside a genuine pod's own directory. Writing a
// knob above pod level would affect every pod nested under it, which is
// exactly the collision this package must not cause.
//
// root is always the caller's configured cgroup root, never derived from
// dir: an earlier version of this guard reconstructed a candidate root by
// walking a fixed number of parents up from dir itself, which made the
// check tautological for any dir whose last path components merely had the
// right shape — including a fake pod cgroup nested inside a real one, where
// the "reconstructed root" would land exactly on the real pod's own
// directory and PodCgroupPath would trivially reproduce dir from it. Fixing
// root to the caller-supplied value closes that hole: PodCgroupPath(root,
// ...) is fully determined before dir is even inspected, so nesting one
// pod-shaped path inside another can never reconstruct to match.
func guardWriteTarget(dir, root string) error {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("%w: %s is not an absolute path", ErrNotPodCgroup, dir)
	}
	cleanRoot := filepath.Clean(root)

	base := filepath.Base(clean)
	for _, driver := range [...]Driver{DriverSystemd, DriverCgroupfs} {
		for _, qos := range [...]QoSClass{QoSGuaranteed, QoSBurstable, QoSBestEffort} {
			uid, ok := extractPodUID(base, driver, qos)
			if !ok {
				continue
			}

			want, err := PodCgroupPath(cleanRoot, driver, qos, uid)
			if err == nil && filepath.Clean(want) == clean {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s", ErrNotPodCgroup, dir)
}
