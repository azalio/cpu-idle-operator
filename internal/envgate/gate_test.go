//go:build linux

package envgate

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

// --- fixture helpers ---------------------------------------------------

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// buildV2Root creates a clean cgroup v2 fixture under t.TempDir(): a
// cgroup.controllers file and, when driver is non-empty, the v2 kubepods
// path matching driver, named kubepodsName. t.TempDir() is always a regular
// filesystem, so this also swaps statfsType to report cgroup2fs for this
// specific root — a real cgroup2 mount cannot be created inside a unit
// test.
func buildV2Root(t *testing.T, driver cgroup.Driver, kubepodsName string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cgroup.controllers"), "cpu io memory pids\n")

	switch driver {
	case cgroup.DriverSystemd:
		mkdir(t, filepath.Join(root, kubepodsName+".slice"))
	case cgroup.DriverCgroupfs:
		mkdir(t, filepath.Join(root, kubepodsName))
	}

	original := statfsType
	statfsType = func(path string) (int64, error) {
		if path == root {
			return unix.CGROUP2_SUPER_MAGIC, nil
		}
		return original(path)
	}
	t.Cleanup(func() { statfsType = original })

	return root
}

// fixedUname returns a UnameFunc that always reports release, for tests
// that need Check to reach the kernel-version check.
func fixedUname(release string) UnameFunc {
	return func() (string, error) { return release, nil }
}

// failIfCalledUname returns a UnameFunc that fails the test if it is ever
// invoked. It proves Check short-circuits on a cgroup-version failure
// before it ever calls uname.
func failIfCalledUname(t *testing.T) UnameFunc {
	t.Helper()
	return func() (string, error) {
		t.Fatal("uname was called even though the cgroup version check should have short-circuited first")
		return "", nil
	}
}

// --- VC1: cgroup version gate --------------------------------------------

func TestVC1CgroupVersionGate(t *testing.T) {
	t.Run("clean v2", func(t *testing.T) {
		root := buildV2Root(t, cgroup.DriverSystemd, cgroup.DefaultKubepodsName)

		result, err := Check(root, cgroup.DefaultKubepodsName, fixedUname("6.17.0-061700-generic"))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if !result.Ready {
			t.Errorf("Ready = false, want true; reason=%v", result.Reason)
		}
		if result.Reason != ReasonOK {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonOK)
		}
		if result.Driver != cgroup.DriverSystemd {
			t.Errorf("Driver = %v, want %v", result.Driver, cgroup.DriverSystemd)
		}
	})

	t.Run("clean v2, non-default kubepods name (kind)", func(t *testing.T) {
		root := buildV2Root(t, cgroup.DriverSystemd, "kubelet-kubepods")

		result, err := Check(root, "kubelet-kubepods", fixedUname("6.17.0-061700-generic"))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if !result.Ready {
			t.Errorf("Ready = false, want true; reason=%v", result.Reason)
		}
		if result.Reason != ReasonOK {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonOK)
		}
		if result.Driver != cgroup.DriverSystemd {
			t.Errorf("Driver = %v, want %v", result.Driver, cgroup.DriverSystemd)
		}
	})

	t.Run("cgroup v1", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "cpu"))

		result, err := Check(root, cgroup.DefaultKubepodsName, failIfCalledUname(t))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if result.Ready {
			t.Error("Ready = true, want false")
		}
		if result.Reason != ReasonCgroupV1 {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonCgroupV1)
		}
	})

	t.Run("cgroup hybrid", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "unified"))

		result, err := Check(root, cgroup.DefaultKubepodsName, failIfCalledUname(t))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if result.Ready {
			t.Error("Ready = true, want false")
		}
		if result.Reason != ReasonCgroupHybrid {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonCgroupHybrid)
		}
	})
}

// TestDetectDriverHonorsConfiguredKubepodsName covers the fix this subtask
// exists for: detectDriver (and, through it, Check) must key off the
// caller-configured kubepods name, not a hardcoded "kubepods" literal —
// otherwise a kind node (whose kubelet actually creates
// "kubelet-kubepods.slice", never plain "kubepods.slice") would keep
// reporting ReasonKubepodsMissing even once --kubepods-name is set
// correctly.
func TestDetectDriverHonorsConfiguredKubepodsName(t *testing.T) {
	t.Run("non-default name present, configured correctly -> ready", func(t *testing.T) {
		root := buildV2Root(t, cgroup.DriverSystemd, "kubelet-kubepods")

		result, err := Check(root, "kubelet-kubepods", fixedUname("6.17.0-061700-generic"))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if !result.Ready {
			t.Errorf("Ready = false, want true; reason=%v", result.Reason)
		}
		if result.Driver != cgroup.DriverSystemd {
			t.Errorf("Driver = %v, want %v", result.Driver, cgroup.DriverSystemd)
		}
	})

	t.Run("non-default name present, checked against the default name -> kubepods_missing", func(t *testing.T) {
		// Reproduces the exact regression this fix closes: a kind-shaped
		// fixture (only "kubelet-kubepods.slice" exists, never plain
		// "kubepods.slice") must still report ReasonKubepodsMissing when
		// asked about the default name -- proving detectDriver looks for
		// the name it was actually given, not a hardcoded one.
		root := buildV2Root(t, cgroup.DriverSystemd, "kubelet-kubepods")

		result, err := Check(root, cgroup.DefaultKubepodsName, failIfCalledUname(t))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if result.Ready {
			t.Error("Ready = true, want false: the default kubepods name does not exist on this fixture")
		}
		if result.Reason != ReasonKubepodsMissing {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonKubepodsMissing)
		}
	})

	t.Run("default name present, configured name absent -> kubepods_missing", func(t *testing.T) {
		root := buildV2Root(t, cgroup.DriverSystemd, cgroup.DefaultKubepodsName)

		result, err := Check(root, "kubelet-kubepods", failIfCalledUname(t))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if result.Ready {
			t.Error("Ready = true, want false: the configured kubepods name does not exist on this fixture")
		}
		if result.Reason != ReasonKubepodsMissing {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonKubepodsMissing)
		}
	})

	t.Run("both driver shapes present for the configured non-default name -> driver_unknown", func(t *testing.T) {
		root := buildV2Root(t, cgroup.DriverSystemd, "kubelet-kubepods")
		mkdir(t, filepath.Join(root, "kubelet-kubepods"))

		result, err := Check(root, "kubelet-kubepods", failIfCalledUname(t))
		if err != nil {
			t.Fatalf("Check returned error: %v", err)
		}
		if result.Ready {
			t.Error("Ready = true, want false")
		}
		if result.Reason != ReasonDriverUnknown {
			t.Errorf("Reason = %v, want %v", result.Reason, ReasonDriverUnknown)
		}
	})
}

// --- VC2: kernel floor -----------------------------------------------------

func TestVC2KernelFloor(t *testing.T) {
	tests := []struct {
		name       string
		release    string
		wantReady  bool
		wantReason Reason
	}{
		{"below floor", "5.14.0", false, ReasonKernelTooOld},
		{"at floor", "5.15.0", true, ReasonOK},
		{"stand kernel with distro suffix", "6.17.0-061700-generic", true, ReasonOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := buildV2Root(t, cgroup.DriverSystemd, cgroup.DefaultKubepodsName)

			result, err := Check(root, cgroup.DefaultKubepodsName, fixedUname(tc.release))
			if err != nil {
				t.Fatalf("Check returned error: %v", err)
			}
			if result.Ready != tc.wantReady {
				t.Errorf("Ready = %v, want %v", result.Ready, tc.wantReady)
			}
			if result.Reason != tc.wantReason {
				t.Errorf("Reason = %v, want %v", result.Reason, tc.wantReason)
			}
		})
	}
}

// --- VC3: zero writes on a failed gate (INV-5) -----------------------------

// fsEntry is a byte-level snapshot of one filesystem entry: its identity
// (path + mode) and, for files, its content and mtime. Comparing slices of
// this before and after a Check call is what makes VC3 an actual proof of
// "no writes" rather than just "Check returned no error".
type fsEntry struct {
	path    string
	mode    fs.FileMode
	content string
	modTime time.Time
}

func snapshotTree(t *testing.T, root string) []fsEntry {
	t.Helper()
	var entries []fsEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		entry := fsEntry{path: p, mode: info.Mode(), modTime: info.ModTime()}
		if !d.IsDir() {
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			entry.content = string(data)
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot tree %s: %v", root, err)
	}
	return entries
}

func TestVC3NoWritesWhenGateFailed(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "cpu"))
	writeFile(t, filepath.Join(root, "cpu", "kubepods.slice", "cpu.idle"), "0")

	before := snapshotTree(t, root)

	result, err := Check(root, cgroup.DefaultKubepodsName, failIfCalledUname(t))
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if result.Ready {
		t.Fatalf("Ready = true, want false: fixture must fail the gate for this test to exercise INV-5 at all")
	}

	after := snapshotTree(t, root)

	if !reflect.DeepEqual(before, after) {
		t.Errorf("cgroup tree under %s changed across a failed Check call:\nbefore: %+v\nafter:  %+v", root, before, after)
	}
}

// --- VC4: cgroupfs is experimental and logs exactly once -------------------

// recordingHandler is a minimal slog.Handler that stores every record it
// receives, so the test can assert on level and message instead of parsing
// formatted log text.
type recordingHandler struct {
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func TestVC4CgroupfsExperimentalWarning(t *testing.T) {
	root := buildV2Root(t, cgroup.DriverCgroupfs, cgroup.DefaultKubepodsName)

	handler := &recordingHandler{}
	original := warnLogger
	warnLogger = slog.New(handler)
	t.Cleanup(func() { warnLogger = original })

	result, err := Check(root, cgroup.DefaultKubepodsName, fixedUname("6.17.0-061700-generic"))
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}

	if result.Driver != cgroup.DriverCgroupfs {
		t.Errorf("Driver = %v, want %v", result.Driver, cgroup.DriverCgroupfs)
	}
	if !result.Experimental {
		t.Error("Experimental = false, want true")
	}

	if len(handler.records) != 1 {
		t.Fatalf("logged %d records, want exactly 1: %+v", len(handler.records), handler.records)
	}
	rec := handler.records[0]
	if rec.Level != slog.LevelWarn {
		t.Errorf("log level = %v, want %v", rec.Level, slog.LevelWarn)
	}
	if !strings.Contains(rec.Message, "experimental") {
		t.Errorf("log message = %q, does not contain %q", rec.Message, "experimental")
	}
}
