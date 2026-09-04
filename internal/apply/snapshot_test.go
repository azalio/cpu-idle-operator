package apply

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/azalio/cpu-idle-operator/internal/cgroup"
)

func TestReadSnapshotRejectsInvalidBooleanAndWeight(t *testing.T) {
	tests := []struct {
		name  string
		knob  string
		value string
	}{
		{name: "invalid idle", knob: KnobCPUIdle, value: "2"},
		{name: "weight above kernel maximum", knob: KnobCPUWeight, value: "10001"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable,
				"abababab-1111-2222-3333-444444444444", "0", "100", "50000 100000", "0")
			if err := os.WriteFile(filepath.Join(dir, tc.knob), []byte(tc.value), 0o644); err != nil {
				t.Fatalf("write %s fixture: %v", tc.knob, err)
			}
			if _, err := ReadSnapshot(dir); err == nil {
				t.Fatalf("ReadSnapshot() error = nil for %s=%q", tc.knob, tc.value)
			}
		})
	}
}

func TestReadSnapshotAcceptsZeroWeightOnlyForIdleCgroup(t *testing.T) {
	root := t.TempDir()
	idleDir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable,
		"abababab-1111-2222-3333-444444444444", "1", "0", "50000 100000", "0")

	snapshot, err := ReadSnapshot(idleDir)
	if err != nil {
		t.Fatalf("ReadSnapshot() for cpu.idle=1 cpu.weight=0: %v", err)
	}
	if !snapshot.IdleActive || snapshot.Weight != 0 {
		t.Fatalf("ReadSnapshot() = %+v, want active idle tier with kernel-reported weight 0", snapshot)
	}

	nonIdleDir := seedPodCgroup(t, root, cgroup.DriverCgroupfs, cgroup.QoSBurstable,
		"cdcdcdcd-1111-2222-3333-444444444444", "0", "0", "50000 100000", "0")
	if _, err := ReadSnapshot(nonIdleDir); err == nil {
		t.Fatal("ReadSnapshot() accepted cpu.weight=0 while cpu.idle=0")
	}
}

func TestParseCPUMaxQuotaValidatesFullKernelShape(t *testing.T) {
	valid := []struct {
		raw      string
		hasQuota bool
		quota    uint64
	}{
		{raw: "max 100000", hasQuota: false},
		{raw: "50000 100000", hasQuota: true, quota: 50000},
	}
	for _, tc := range valid {
		hasQuota, quota, err := parseCPUMaxQuota(tc.raw)
		if err != nil || hasQuota != tc.hasQuota || quota != tc.quota {
			t.Fatalf("parseCPUMaxQuota(%q) = (%v, %d, %v), want (%v, %d, nil)", tc.raw, hasQuota, quota, err, tc.hasQuota, tc.quota)
		}
	}

	for _, raw := range []string{
		"", "max", "max 100000 extra", "0 100000", "999 100000",
		"1000 999", "1000 1000001", "-1 100000", "wat 100000",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := parseCPUMaxQuota(raw); err == nil {
				t.Fatalf("parseCPUMaxQuota(%q) error = nil", raw)
			}
		})
	}
}
