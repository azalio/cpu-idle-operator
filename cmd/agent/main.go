// Command agent is the cpi-idle-operator node agent entry point. Startup
// order is fixed: parse flags, run the environment gate check, then start
// the metrics and health HTTP servers — everything past flag parsing lives
// in internal/agent.Lifecycle, which this file only wires together and
// runs. On SIGTERM/SIGINT the process cancels its context, lets
// Lifecycle.Run drain in flight work and close its servers, and exits;
// there is no shutdown hook here that touches a cgroup or calls Revert
// (INV-4) — see internal/agent/lifecycle.go's doc comment for why.
//
// --revert-all is the one sanctioned exception to INV-4 (resolution
// T-007): a human explicitly runs it, as a standalone Job, right before
// the operator is removed from the cluster. main routes it to
// agent.RunRevertAll before Lifecycle ever starts, and exits without
// touching Lifecycle's informer/HTTP-server loop at all.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/azalio/cpi-idle-operator/internal/agent"
	"github.com/azalio/cpi-idle-operator/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger := slog.Default()

	cfg, err := config.ParseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "cpi-idle-agent: %v\n", err)
		os.Exit(1)
	}

	client, err := newKubeClient()
	if err != nil {
		logger.Error("cpi-idle-agent: build kubernetes client", "error", err)
		os.Exit(1)
	}

	if cfg.RevertAll {
		if err := agent.RunRevertAll(ctx, cfg, agent.RevertAllOptions{Client: client, Logger: logger}); err != nil {
			logger.Error("cpi-idle-agent: revert-all failed", "error", err)
			os.Exit(1)
		}
		return
	}

	lc := &agent.Lifecycle{
		Client: client,
		Config: cfg,
		Logger: logger,
	}
	if err := lc.Run(ctx); err != nil {
		logger.Error("cpi-idle-agent: exited with error", "error", err)
		os.Exit(1)
	}
}

// newKubeClient builds the Kubernetes API client this agent uses in
// production: in-cluster config, since a DaemonSet always runs inside the
// cluster it watches — there is no supported out-of-cluster deployment for
// this operator to fall back to a kubeconfig for.
func newKubeClient() (kubernetes.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return kubernetes.NewForConfig(restCfg)
}
