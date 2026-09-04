package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVC3DefaultsAndOverrides(t *testing.T) {
	t.Run("defaults from env node name", func(t *testing.T) {
		t.Setenv("NODE_NAME", "node-a")

		cfg, err := ParseFlags(nil)
		if err != nil {
			t.Fatalf("ParseFlags() error = %v, want nil", err)
		}

		if cfg.CgroupRoot != "/sys/fs/cgroup" {
			t.Errorf("CgroupRoot = %q, want /sys/fs/cgroup", cfg.CgroupRoot)
		}
		if cfg.KubepodsName != "kubepods" {
			t.Errorf("KubepodsName = %q, want kubepods", cfg.KubepodsName)
		}
		if cfg.NodeName != "node-a" {
			t.Errorf("NodeName = %q, want node-a (from NODE_NAME env)", cfg.NodeName)
		}
		if cfg.ResyncPeriod != 60*time.Second {
			t.Errorf("ResyncPeriod = %v, want 60s", cfg.ResyncPeriod)
		}
		if cfg.RevertAll {
			t.Errorf("RevertAll = true, want false by default")
		}
		if cfg.MetricsAddr == "" {
			t.Errorf("MetricsAddr must have a non-empty default")
		}
		if cfg.HealthAddr == "" {
			t.Errorf("HealthAddr must have a non-empty default")
		}
	})

	t.Run("flag overrides win over env and defaults", func(t *testing.T) {
		t.Setenv("NODE_NAME", "node-from-env")

		cfg, err := ParseFlags([]string{
			"--cgroup-root=/custom/cgroup",
			"--kubepods-name=kubelet-kubepods",
			"--node-name=node-from-flag",
			"--resync-period=30s",
			"--revert-all",
			"--metrics-addr=:9090",
			"--health-addr=:9091",
		})
		if err != nil {
			t.Fatalf("ParseFlags() error = %v, want nil", err)
		}

		if cfg.CgroupRoot != "/custom/cgroup" {
			t.Errorf("CgroupRoot = %q, want /custom/cgroup", cfg.CgroupRoot)
		}
		if cfg.KubepodsName != "kubelet-kubepods" {
			t.Errorf("KubepodsName = %q, want kubelet-kubepods", cfg.KubepodsName)
		}
		if cfg.NodeName != "node-from-flag" {
			t.Errorf("NodeName = %q, want node-from-flag (flag beats env)", cfg.NodeName)
		}
		if cfg.ResyncPeriod != 30*time.Second {
			t.Errorf("ResyncPeriod = %v, want 30s", cfg.ResyncPeriod)
		}
		if !cfg.RevertAll {
			t.Errorf("RevertAll = false, want true")
		}
		if cfg.MetricsAddr != ":9090" {
			t.Errorf("MetricsAddr = %q, want :9090", cfg.MetricsAddr)
		}
		if cfg.HealthAddr != ":9091" {
			t.Errorf("HealthAddr = %q, want :9091", cfg.HealthAddr)
		}
	})

	t.Run("empty node name is an explicit startup error", func(t *testing.T) {
		t.Setenv("NODE_NAME", "")

		_, err := ParseFlags(nil)
		if !errors.Is(err, ErrEmptyNodeName) {
			t.Fatalf("ParseFlags() error = %v, want ErrEmptyNodeName", err)
		}
	})

	t.Run("empty node name via explicit flag also errors", func(t *testing.T) {
		t.Setenv("NODE_NAME", "node-from-env")

		_, err := ParseFlags([]string{"--node-name="})
		if !errors.Is(err, ErrEmptyNodeName) {
			t.Fatalf("ParseFlags() error = %v, want ErrEmptyNodeName", err)
		}
	})
}

func TestGuardFlagsRejectUnsafeValues(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "negative high", args: []string{"--guard-high=-0.1"}, want: "--guard-high"},
		{name: "nan high", args: []string{"--guard-high=NaN"}, want: "--guard-high"},
		{name: "nan low", args: []string{"--guard-high=0.7", "--guard-low=NaN"}, want: "--guard-low"},
		{name: "zero period", args: []string{"--guard-high=0.7", "--guard-period=0s"}, want: "--guard-period"},
		{name: "negative period", args: []string{"--guard-high=0.7", "--guard-period=-1s"}, want: "--guard-period"},
		{name: "malformed floor", args: []string{"--guard-high=0.7", "--guard-floor=wat"}, want: "--guard-floor"},
		{name: "unbounded floor", args: []string{"--guard-high=0.7", "--guard-floor=max 100000"}, want: "--guard-floor"},
		{name: "zero quota", args: []string{"--guard-high=0.7", "--guard-floor=0 100000"}, want: "--guard-floor"},
		{name: "period below kernel minimum", args: []string{"--guard-high=0.7", "--guard-floor=1000 999"}, want: "--guard-floor"},
		{name: "removed freeze mode", args: []string{"--guard-freeze=true"}, want: "flag provided but not defined"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFlags(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseFlags(%v) error = %v, want error containing %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestParseFlagsCanonicalizesGuardFloor(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")
	cfg, err := ParseFlags([]string{"--guard-floor= 010000   100000 "})
	if err != nil {
		t.Fatalf("ParseFlags() error = %v", err)
	}
	if cfg.GuardFloor != "10000 100000" {
		t.Fatalf("GuardFloor = %q, want canonical cpu.max value", cfg.GuardFloor)
	}
}

func TestParseFlagsRejectsUnsafeCoreValues(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "zero resync", args: []string{"--resync-period=0s"}, want: "--resync-period"},
		{name: "negative resync", args: []string{"--resync-period=-1s"}, want: "--resync-period"},
		{name: "relative cgroup root", args: []string{"--cgroup-root=relative"}, want: "--cgroup-root"},
		{name: "empty kubepods name", args: []string{"--kubepods-name="}, want: "--kubepods-name"},
		{name: "traversing kubepods name", args: []string{"--kubepods-name=../kubepods"}, want: "--kubepods-name"},
		{name: "positional argument", args: []string{"surprise"}, want: "unexpected positional"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFlags(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseFlags(%v) error = %v, want error containing %q", tc.args, err, tc.want)
			}
		})
	}
}
