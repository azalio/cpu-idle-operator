package cgroup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadKnobHappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.idle"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("seed knob file: %v", err)
	}

	got, err := ReadKnob(dir, "cpu.idle")
	if err != nil {
		t.Fatalf("ReadKnob returned error: %v", err)
	}
	if got != "1" {
		t.Errorf("ReadKnob = %q, want %q", got, "1")
	}
}

func TestReadKnobMissingDirReturnsCgroupGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := ReadKnob(dir, "cpu.idle")
	if !errors.Is(err, ErrCgroupGone) {
		t.Errorf("ReadKnob error = %v, want ErrCgroupGone", err)
	}
}

func TestWriteKnobHappyPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.idle"), []byte("0"), 0o644); err != nil {
		t.Fatalf("seed knob file: %v", err)
	}

	if err := WriteKnob(root, DefaultKubepodsName, dir, "cpu.idle", "1"); err != nil {
		t.Fatalf("WriteKnob returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "cpu.idle"))
	if err != nil {
		t.Fatalf("read back knob file: %v", err)
	}
	if string(got) != "1" {
		t.Errorf("knob file content = %q, want %q", got, "1")
	}
}

func TestWriteKnobMissingDirReturnsCgroupGone(t *testing.T) {
	// "kubepods/pod123" is a valid cgroupfs Guaranteed-QoS pod-level path
	// shape (root/kubepods/pod<uid>), so the guard lets this through and
	// the ENOENT must surface as ErrCgroupGone: the pod disappeared between
	// the caller reading its cache and writing here.
	root := t.TempDir()
	dir := filepath.Join(root, "kubepods", "pod123")

	err := WriteKnob(root, DefaultKubepodsName, dir, "cpu.idle", "1")
	if !errors.Is(err, ErrCgroupGone) {
		t.Errorf("WriteKnob error = %v, want ErrCgroupGone", err)
	}
}

// fakeKnobWriter lets tests control Write and Close independently, which a
// real filesystem cannot reliably do: forcing a real cgroup knob file to
// fail exactly on Close (and succeed on Write) requires a live kernel
// rejecting the value, which is not reproducible under t.TempDir().
type fakeKnobWriter struct {
	writeErr error
	closeErr error
}

func (f *fakeKnobWriter) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeKnobWriter) Close() error {
	return f.closeErr
}

func withFakeKnobWriter(t *testing.T, w *fakeKnobWriter) {
	t.Helper()
	original := openKnobWriter
	openKnobWriter = func(path string) (knobWriter, error) {
		return w, nil
	}
	t.Cleanup(func() { openKnobWriter = original })
}

// TestVC2CloseErrorSurfaced proves the case this package exists to catch:
// the kernel can accept a Write() on a cgroup knob and only report the
// rejection when the descriptor is closed (observed on the stand as EINVAL
// on cpu.weight while cpu.idle=1). WriteKnob must not swallow that error.
func TestVC2CloseErrorSurfaced(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubepods", "pod550e8400_e29b_41d4_a716_446655440000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	closeErr := &fs.PathError{Op: "close", Path: "cpu.weight", Err: syscall.EINVAL}
	withFakeKnobWriter(t, &fakeKnobWriter{closeErr: closeErr})

	err := WriteKnob(root, DefaultKubepodsName, dir, "cpu.weight", "20")
	if err == nil {
		t.Fatal("expected WriteKnob to return the Close error, got nil")
	}
	if !errors.Is(err, syscall.EINVAL) {
		t.Errorf("errors.Is(err, syscall.EINVAL) = false, want true; err = %v", err)
	}
}

// TestVC2CloseErrorTakesPriorityOverWriteError proves the priority ordering
// required by INV-3: when both Write and Close fail, the Close error is the
// one that must reach the caller.
func TestVC2CloseErrorTakesPriorityOverWriteError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubepods", "pod550e8400_e29b_41d4_a716_446655440000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeErr := errors.New("write: boom")
	closeErr := &fs.PathError{Op: "close", Path: "cpu.weight", Err: syscall.EINVAL}
	withFakeKnobWriter(t, &fakeKnobWriter{writeErr: writeErr, closeErr: closeErr})

	err := WriteKnob(root, DefaultKubepodsName, dir, "cpu.weight", "20")
	if !errors.Is(err, syscall.EINVAL) {
		t.Errorf("errors.Is(err, syscall.EINVAL) = false, want true; err = %v", err)
	}
	if errors.Is(err, writeErr) {
		t.Errorf("write error leaked into result even though Close also failed: %v", err)
	}
}

// TestVC3WriteTargetGuard proves INV-1: WriteKnob refuses to write above
// pod level. Three categories of non-pod cgroup: the kubepods root, a
// QoS-level slice/directory (checked for both driver naming schemes), and
// a container scope. Every case is repeated with a non-default kubepods
// name (kind's measured "kubelet-kubepods") to prove the guard rejects the
// same shapes regardless of which name it is configured with.
func TestVC3WriteTargetGuard(t *testing.T) {
	base := t.TempDir()

	cases := []struct {
		name         string
		kubepodsName string
		dir          string
	}{
		{"kubepods root, systemd", DefaultKubepodsName, filepath.Join(base, "kubepods.slice")},
		{"kubepods root, cgroupfs", DefaultKubepodsName, filepath.Join(base, "kubepods")},
		{"QoS slice, systemd", DefaultKubepodsName, filepath.Join(base, "kubepods.slice", "kubepods-burstable.slice")},
		{"QoS dir, cgroupfs", DefaultKubepodsName, filepath.Join(base, "kubepods", "burstable")},
		{"container scope", DefaultKubepodsName, filepath.Join(base, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice", "cri-containerd-abc123.scope")},
		{"kubepods root, systemd, non-default name", "kubelet-kubepods", filepath.Join(base, "kubelet-kubepods.slice")},
		{"kubepods root, cgroupfs, non-default name", "kubelet-kubepods", filepath.Join(base, "kubelet-kubepods")},
		{"QoS slice, systemd, non-default name", "kubelet-kubepods",
			filepath.Join(base, "kubelet-kubepods.slice", "kubelet-kubepods-burstable.slice")},
		{"container scope, non-default name", "kubelet-kubepods",
			filepath.Join(base, "kubelet-kubepods.slice", "kubelet-kubepods-burstable.slice",
				"kubelet-kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice", "cri-containerd-abc123.scope")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WriteKnob(base, tc.kubepodsName, tc.dir, "cpu.idle", "1")
			if !errors.Is(err, ErrNotPodCgroup) {
				t.Errorf("WriteKnob(%q, %q) error = %v, want ErrNotPodCgroup", tc.kubepodsName, tc.dir, err)
			}
		})
	}
}

// TestWriteKnobAllowsPodLevelDir is the guard's negative case: a genuine
// pod-level directory must pass the guard, for both the default kubepods
// name and a non-default one. The write itself still fails here (no such
// file), but with ErrCgroupGone rather than ErrNotPodCgroup — proof the
// guard did not fire.
func TestWriteKnobAllowsPodLevelDir(t *testing.T) {
	cases := []struct {
		name         string
		kubepodsName string
		dir          func(root string) string
	}{
		{"default kubepods name", DefaultKubepodsName, func(root string) string {
			return filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
				"kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice")
		}},
		{"non-default kubepods name", "kubelet-kubepods", func(root string) string {
			return filepath.Join(root, "kubelet-kubepods.slice", "kubelet-kubepods-burstable.slice",
				"kubelet-kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := tc.dir(root)

			err := WriteKnob(root, tc.kubepodsName, dir, "cpu.idle", "1")
			if errors.Is(err, ErrNotPodCgroup) {
				t.Errorf("WriteKnob(%q, %q) incorrectly rejected as non-pod cgroup: %v", tc.kubepodsName, dir, err)
			}
			if !errors.Is(err, ErrCgroupGone) {
				t.Errorf("WriteKnob(%q, %q) error = %v, want ErrCgroupGone", tc.kubepodsName, dir, err)
			}
		})
	}
}

// TestGuardWriteTargetRejectsNonAllowListedDirs proves the guard is a true
// allow-list rather than a block-list: each case here is a directory a
// block-list (reject known-bad names, accept everything else) would have
// let through, because none of them match a rejected name literally.
func TestGuardWriteTargetRejectsNonAllowListedDirs(t *testing.T) {
	t.Run("arbitrary directory", func(t *testing.T) {
		root := t.TempDir()
		if err := guardWriteTarget(t.TempDir(), root, DefaultKubepodsName); !errors.Is(err, ErrNotPodCgroup) {
			t.Errorf("guardWriteTarget(arbitrary dir) error = %v, want ErrNotPodCgroup", err)
		}
	})

	t.Run("non-pod subdirectory inside a real QoS slice", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "not-a-pod-at-all")
		if err := guardWriteTarget(dir, root, DefaultKubepodsName); !errors.Is(err, ErrNotPodCgroup) {
			t.Errorf("guardWriteTarget(%q) error = %v, want ErrNotPodCgroup", dir, err)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join("kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice")
		if err := guardWriteTarget(dir, root, DefaultKubepodsName); !errors.Is(err, ErrNotPodCgroup) {
			t.Errorf("guardWriteTarget(%q) error = %v, want ErrNotPodCgroup", dir, err)
		}
	})

	t.Run("path traversal escapes the pod cgroup", func(t *testing.T) {
		// Built by string concatenation, not filepath.Join, so the ".."
		// components are still literally present when guardWriteTarget
		// receives dir — its own filepath.Clean must be what defeats them,
		// not the caller having already normalized the path.
		base := t.TempDir()
		dir := base + "/kubepods.slice/kubepods-burstable.slice/" +
			"kubepods-burstable-pod550e8400_e29b_41d4_a716_446655440000.slice/../../../etc/passwd"
		if err := guardWriteTarget(dir, base, DefaultKubepodsName); !errors.Is(err, ErrNotPodCgroup) {
			t.Errorf("guardWriteTarget(%q) error = %v, want ErrNotPodCgroup", dir, err)
		}
	})

	t.Run("non-pod subdirectory inside a real QoS slice, non-default kubepods name", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "kubelet-kubepods.slice", "kubelet-kubepods-burstable.slice", "not-a-pod-at-all")
		if err := guardWriteTarget(dir, root, "kubelet-kubepods"); !errors.Is(err, ErrNotPodCgroup) {
			t.Errorf("guardWriteTarget(%q) error = %v, want ErrNotPodCgroup", dir, err)
		}
	})

	// TestGuardWriteTargetRejectsPodNestedInsideRealPodCgroup is the
	// regression this function exists to catch (code review 002 follow-up):
	// a directory shaped like a second, fake pod cgroup nested inside a
	// genuine pod's own directory. Before root was a caller-supplied
	// parameter, guardWriteTarget derived its expected root by walking a
	// fixed number of parents up from dir, so this self-similar nesting
	// reconstructed the *real* pod's own directory as a plausible "root"
	// and PodCgroupPath trivially reproduced dir from it — the check was
	// tautological. With root fixed to the caller's actual configured
	// cgroup root, PodCgroupPath(root, ...) is fully determined before dir
	// is inspected, so this can never match.
	t.Run("pod cgroup nested inside a real pod's own directory", func(t *testing.T) {
		root := t.TempDir()
		const realUID = "550e8400-e29b-41d4-a716-446655440000"
		realPod, err := PodCgroupPath(root, DefaultKubepodsName, DriverCgroupfs, QoSBurstable, realUID)
		if err != nil {
			t.Fatalf("PodCgroupPath: %v", err)
		}
		fakeNestedPod := filepath.Join(realPod, "kubepods", "burstable", "podBBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB")

		if err := guardWriteTarget(fakeNestedPod, root, DefaultKubepodsName); !errors.Is(err, ErrNotPodCgroup) {
			t.Errorf("guardWriteTarget(%q, root=%q) error = %v, want ErrNotPodCgroup", fakeNestedPod, root, err)
		}
	})
}

// TestGuardWriteTargetAllowsGenuinePodCgroupPaths proves the positive side
// of the allow-list: every path PodCgroupPath computes, for both drivers,
// all three QoS classes, and both the default and a non-default kubepods
// name, must pass the guard.
func TestGuardWriteTargetAllowsGenuinePodCgroupPaths(t *testing.T) {
	root := t.TempDir()
	const podUID = "550e8400-e29b-41d4-a716-446655440000"

	for _, kubepodsName := range []string{DefaultKubepodsName, "kubelet-kubepods"} {
		for _, driver := range []Driver{DriverSystemd, DriverCgroupfs} {
			for _, qos := range []QoSClass{QoSGuaranteed, QoSBurstable, QoSBestEffort} {
				t.Run(kubepodsName+"/"+string(driver)+"/"+string(qos), func(t *testing.T) {
					dir, err := PodCgroupPath(root, kubepodsName, driver, qos, podUID)
					if err != nil {
						t.Fatalf("PodCgroupPath(%q, %q, %q): %v", kubepodsName, driver, qos, err)
					}
					if err := guardWriteTarget(dir, root, kubepodsName); err != nil {
						t.Errorf("guardWriteTarget(%q, root=%q, kubepodsName=%q) = %v, want nil", dir, root, kubepodsName, err)
					}
				})
			}
		}
	}
}

// TestVC4NoRuntimeAccess enforces HC-3 across the whole internal/ tree, not
// just this package: no /proc access, no CRI socket, no cri-api import.
// Forbidden literals are assembled from parts so this test does not trip
// over its own source.
func TestVC4NoRuntimeAccess(t *testing.T) {
	forbiddenProc := "/" + "proc" + "/"
	forbiddenCRISocket := "unix://" + "/run/containerd"
	forbiddenCRIImport := "k8s.io/" + "cri-api"

	root := filepath.Join("..") // internal/

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		content := string(data)
		if strings.Contains(content, forbiddenProc) {
			t.Errorf("%s references %q", p, forbiddenProc)
		}
		if strings.Contains(content, forbiddenCRISocket) {
			t.Errorf("%s references %q", p, forbiddenCRISocket)
		}
		if strings.Contains(content, forbiddenCRIImport) {
			t.Errorf("%s imports %q", p, forbiddenCRIImport)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}
