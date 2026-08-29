// Package config parses the cpi-idle-operator agent's command-line flags
// into a validated Config value.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"
)

const (
	defaultCgroupRoot = "/sys/fs/cgroup"
	// defaultKubepodsName is the top-level kubepods cgroup name a stock
	// kubelet creates. A kubelet started with a non-default --cgroup-root
	// (e.g. kind, which uses "/kubelet") instead prefixes every kubepods
	// slice/directory name with its own root's basename (e.g.
	// "kubelet-kubepods") — see internal/cgroup.PodCgroupPath's doc comment
	// for the mechanics --kubepods-name exists to override.
	defaultKubepodsName = "kubepods"
	defaultResyncPeriod = 60 * time.Second
	defaultMetricsAddr  = ":8080"
	defaultHealthAddr   = ":8081"
	defaultGuardLow     = 0.60
	defaultGuardPeriod  = 5 * time.Second
	defaultGuardFloor   = "10000 100000"
	nodeNameEnvVar      = "NODE_NAME"
)

// ErrEmptyNodeName is returned when neither --node-name nor the NODE_NAME
// environment variable identify the node this agent runs on. The agent must
// scope its pod watch to a single node, so a missing node name is a startup
// error rather than a silent fallback to watching every node.
var ErrEmptyNodeName = errors.New("node name is empty: pass --node-name or set NODE_NAME")

// Config holds the agent's runtime configuration.
type Config struct {
	// CgroupRoot is the filesystem root under which pod cgroups are located.
	CgroupRoot string
	// KubepodsName is the top-level kubepods cgroup slice/directory name
	// kubelet actually uses under CgroupRoot. Defaults to "kubepods" (a
	// stock kubelet); a kubelet started with a non-default --cgroup-root
	// (e.g. kind's "/kubelet") needs the matching prefixed name instead
	// (e.g. "kubelet-kubepods"), paired with a CgroupRoot pointed at that
	// same kubelet-root slice.
	KubepodsName string
	// NodeName scopes the pod watch to this node; it must never be empty.
	NodeName string
	// ResyncPeriod is the informer's full resync interval.
	ResyncPeriod time.Duration
	// RevertAll runs a one-shot revert of all tiers on this node, then exits.
	RevertAll bool
	// MetricsAddr is the listen address for the Prometheus metrics endpoint.
	MetricsAddr string
	// HealthAddr is the listen address for the health/readiness endpoint.
	HealthAddr string
	// GuardHigh enables the node guard when positive: the non-idle CPU
	// utilization fraction above which idle-tier pods are suppressed.
	GuardHigh float64
	// GuardLow is the fraction below which suppression is lifted.
	GuardLow float64
	// GuardPeriod is the guard's sampling interval.
	GuardPeriod time.Duration
	// GuardFloor is the cpu.max value written while suppressed.
	GuardFloor string
}

// ParseFlags parses argv (excluding the program name) into a Config,
// applying defaults and falling back to the NODE_NAME environment variable
// for --node-name. It returns ErrEmptyNodeName if the resolved node name is
// empty, since watching every node in the cluster is never an acceptable
// silent fallback.
func ParseFlags(argv []string) (Config, error) {
	fs := flag.NewFlagSet("cpi-idle-agent", flag.ContinueOnError)

	cgroupRoot := fs.String("cgroup-root", defaultCgroupRoot, "filesystem root under which pod cgroups are located")
	kubepodsName := fs.String("kubepods-name", defaultKubepodsName, "top-level kubepods cgroup slice/directory name kubelet uses under --cgroup-root (non-default on e.g. kind: see README)")
	nodeName := fs.String("node-name", os.Getenv(nodeNameEnvVar), fmt.Sprintf("node this agent watches (default: %s env var)", nodeNameEnvVar))
	resyncPeriod := fs.Duration("resync-period", defaultResyncPeriod, "informer full resync interval")
	revertAll := fs.Bool("revert-all", false, "run a one-shot revert of all tiers on this node, then exit")
	metricsAddr := fs.String("metrics-addr", defaultMetricsAddr, "listen address for the Prometheus metrics endpoint")
	healthAddr := fs.String("health-addr", defaultHealthAddr, "listen address for the health/readiness endpoint")
	guardHigh := fs.Float64("guard-high", 0, "node guard: non-idle CPU utilization fraction above which idle-tier pods are suppressed (0 disables the guard)")
	guardLow := fs.Float64("guard-low", defaultGuardLow, "node guard: fraction below which suppression is lifted")
	guardPeriod := fs.Duration("guard-period", defaultGuardPeriod, "node guard: sampling interval")
	guardFloor := fs.String("guard-floor", defaultGuardFloor, "node guard: cpu.max value written to suppressed idle-tier pods")

	if err := fs.Parse(argv); err != nil {
		return Config{}, err
	}

	if *nodeName == "" {
		return Config{}, ErrEmptyNodeName
	}

	if *guardHigh > 0 {
		if *guardHigh > 1 {
			return Config{}, fmt.Errorf("--guard-high must be in (0, 1], got %v", *guardHigh)
		}
		if *guardLow <= 0 || *guardLow >= *guardHigh {
			return Config{}, fmt.Errorf("--guard-low must be in (0, --guard-high), got %v", *guardLow)
		}
	}

	return Config{
		CgroupRoot:   *cgroupRoot,
		KubepodsName: *kubepodsName,
		NodeName:     *nodeName,
		ResyncPeriod: *resyncPeriod,
		RevertAll:    *revertAll,
		MetricsAddr:  *metricsAddr,
		HealthAddr:   *healthAddr,
		GuardHigh:    *guardHigh,
		GuardLow:     *guardLow,
		GuardPeriod:  *guardPeriod,
		GuardFloor:   *guardFloor,
	}, nil
}
