package config

import (
	"errors"
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
