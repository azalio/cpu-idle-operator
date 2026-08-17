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
	defaultCgroupRoot   = "/sys/fs/cgroup"
	defaultResyncPeriod = 60 * time.Second
	defaultMetricsAddr  = ":8080"
	defaultHealthAddr   = ":8081"
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
}

// ParseFlags parses argv (excluding the program name) into a Config,
// applying defaults and falling back to the NODE_NAME environment variable
// for --node-name. It returns ErrEmptyNodeName if the resolved node name is
// empty, since watching every node in the cluster is never an acceptable
// silent fallback.
func ParseFlags(argv []string) (Config, error) {
	fs := flag.NewFlagSet("cpi-idle-agent", flag.ContinueOnError)

	cgroupRoot := fs.String("cgroup-root", defaultCgroupRoot, "filesystem root under which pod cgroups are located")
	nodeName := fs.String("node-name", os.Getenv(nodeNameEnvVar), fmt.Sprintf("node this agent watches (default: %s env var)", nodeNameEnvVar))
	resyncPeriod := fs.Duration("resync-period", defaultResyncPeriod, "informer full resync interval")
	revertAll := fs.Bool("revert-all", false, "run a one-shot revert of all tiers on this node, then exit")
	metricsAddr := fs.String("metrics-addr", defaultMetricsAddr, "listen address for the Prometheus metrics endpoint")
	healthAddr := fs.String("health-addr", defaultHealthAddr, "listen address for the health/readiness endpoint")

	if err := fs.Parse(argv); err != nil {
		return Config{}, err
	}

	if *nodeName == "" {
		return Config{}, ErrEmptyNodeName
	}

	return Config{
		CgroupRoot:   *cgroupRoot,
		NodeName:     *nodeName,
		ResyncPeriod: *resyncPeriod,
		RevertAll:    *revertAll,
		MetricsAddr:  *metricsAddr,
		HealthAddr:   *healthAddr,
	}, nil
}
