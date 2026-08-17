package cgroup

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// TestVC5PodCgroupPathTable covers both cgroup drivers, all three QoS
// classes, and a pod UID both with dashes (the real Kubernetes UID format)
// and without (an edge case that must not be escaped further). The
// Burstable/systemd/dashed-UID case is the exact path measured on the
// stand: root/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid_>.slice.
func TestVC5PodCgroupPathTable(t *testing.T) {
	root := t.TempDir()

	const (
		uidDashed    = "550e8400-e29b-41d4-a716-446655440000"
		uidUnescaped = "550e8400_e29b_41d4_a716_446655440000" // dashes -> underscores
		uidNoDashes  = "550e8400e29b41d4a716446655440000"
	)

	tests := []struct {
		name       string
		driver     Driver
		qos        QoSClass
		uid        string
		wantSuffix string
	}{
		// systemd, dashed UID
		{"systemd/guaranteed/dashed", DriverSystemd, QoSGuaranteed, uidDashed,
			"kubepods.slice/kubepods-pod" + uidUnescaped + ".slice"},
		{"systemd/burstable/dashed", DriverSystemd, QoSBurstable, uidDashed,
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + uidUnescaped + ".slice"},
		{"systemd/besteffort/dashed", DriverSystemd, QoSBestEffort, uidDashed,
			"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + uidUnescaped + ".slice"},
		// systemd, no-dash UID: nothing to escape, output equals input.
		{"systemd/guaranteed/nodash", DriverSystemd, QoSGuaranteed, uidNoDashes,
			"kubepods.slice/kubepods-pod" + uidNoDashes + ".slice"},
		{"systemd/burstable/nodash", DriverSystemd, QoSBurstable, uidNoDashes,
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + uidNoDashes + ".slice"},
		{"systemd/besteffort/nodash", DriverSystemd, QoSBestEffort, uidNoDashes,
			"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + uidNoDashes + ".slice"},
		// cgroupfs, dashed UID: dashes are NOT replaced.
		{"cgroupfs/guaranteed/dashed", DriverCgroupfs, QoSGuaranteed, uidDashed,
			"kubepods/pod" + uidDashed},
		{"cgroupfs/burstable/dashed", DriverCgroupfs, QoSBurstable, uidDashed,
			"kubepods/burstable/pod" + uidDashed},
		{"cgroupfs/besteffort/dashed", DriverCgroupfs, QoSBestEffort, uidDashed,
			"kubepods/besteffort/pod" + uidDashed},
		// cgroupfs, no-dash UID.
		{"cgroupfs/guaranteed/nodash", DriverCgroupfs, QoSGuaranteed, uidNoDashes,
			"kubepods/pod" + uidNoDashes},
		{"cgroupfs/burstable/nodash", DriverCgroupfs, QoSBurstable, uidNoDashes,
			"kubepods/burstable/pod" + uidNoDashes},
		{"cgroupfs/besteffort/nodash", DriverCgroupfs, QoSBestEffort, uidNoDashes,
			"kubepods/besteffort/pod" + uidNoDashes},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PodCgroupPath(root, tc.driver, tc.qos, tc.uid)
			if err != nil {
				t.Fatalf("PodCgroupPath(%q, %q, %q) returned error: %v", tc.driver, tc.qos, tc.uid, err)
			}
			want := path.Join(root, tc.wantSuffix)
			if got != want {
				t.Errorf("PodCgroupPath(%q, %q, %q) = %q, want %q", tc.driver, tc.qos, tc.uid, got, want)
			}
		})
	}
}

func TestPodCgroupPathRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()

	if _, err := PodCgroupPath(root, Driver("bogus"), QoSBurstable, "uid"); err == nil {
		t.Error("expected error for unknown driver, got nil")
	}
	if _, err := PodCgroupPath(root, DriverSystemd, QoSClass("bogus"), "uid"); err == nil {
		t.Error("expected error for unknown QoS class, got nil")
	}
	if _, err := PodCgroupPath(root, DriverSystemd, QoSBurstable, ""); err == nil {
		t.Error("expected error for empty pod UID, got nil")
	}
}

// TestVC1NoHardcodedRoot enforces VC1/CCR-3: every exported function in
// this package takes the cgroup root as a parameter, so no file here may
// hardcode the real cgroup root path. The forbidden literal is assembled
// from parts so this test does not trip over its own source.
func TestVC1NoHardcodedRoot(t *testing.T) {
	forbidden := "/" + "sys" + "/" + "fs" + "/" + "cgroup"

	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
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
		if strings.Contains(string(data), forbidden) {
			t.Errorf("%s contains hardcoded cgroup root %q", p, forbidden)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/cgroup: %v", err)
	}
}
