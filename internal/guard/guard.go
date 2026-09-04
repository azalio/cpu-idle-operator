// Package guard implements the node guard: a per-node control loop that
// suppresses idle-tier pods' CPU while the node's non-idle load is above a
// threshold, and lets them run again once it drops back.
//
// Why this exists: cpu.idle removes a neighbor from *time-sharing*
// arbitration, but not from wakeup-placement heuristics (SIS_UTIL's scan
// depth is computed from PELT utilization, which cannot tell idle cycles
// from normal ones), not from SMT/LLC sharing, and not from the service's
// own queueing once the node runs near its capacity. Measured on a
// saturated node, an idle-tier stress-ng that was only getting 0.2 cores
// still tripled the foreground's p99 — while harvesting less than 3% of
// the node. Past ~70% non-idle utilization there is little left worth
// harvesting, so this guard temporarily caps the idle tier at a small,
// explicitly configured cpu.max floor until the pressure passes.
//
// Before changing a cgroup knob, the guard persists an ownership marker and
// the exact old value on the Pod. That makes cleanup deterministic across
// annotation removal, eligibility changes, process restarts, and guard
// configuration changes; it never guesses that a value merely resembling
// its floor belongs to it.
package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/azalio/cpu-idle-operator/internal/annotations"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// Config carries the guard's tunables, parsed from the agent's flags.
type Config struct {
	// High is the non-idle utilization fraction (0..1] above which the
	// guard suppresses idle-tier pods. Zero or negative disables the guard.
	High float64
	// Low is the fraction below which suppression is lifted. Must be
	// below High; the gap is the hysteresis band.
	Low float64
	// Period is the sampling interval.
	Period time.Duration
	// FloorQuota is the cpu.max value written while suppressed, e.g.
	// "10000 100000" for 10ms of CPU per 100ms period.
	FloorQuota   string
	CgroupRoot   string
	KubepodsName string
	Driver       cgroup.Driver
	NodeName     string
}

// Enabled reports whether the parsed configuration turns the guard on.
func (c Config) Enabled() bool { return c.High > 0 }

// Enabled reports whether this Guard may create new suppressions. Recovery
// remains available regardless of this value.
func (g *Guard) Enabled() bool { return g.cfg.Enabled() }

// decider is the hysteresis state machine. A transition needs two
// consecutive samples on the far side of the threshold, so a single noisy
// sample cannot flap multi-pod cgroup writes.
type decider struct {
	hot    bool
	streak int
}

// observe feeds one utilization sample and reports the (possibly new)
// temperature.
func (d *decider) observe(util, high, low float64) bool {
	crossed := (!d.hot && util > high) || (d.hot && util < low)
	if crossed {
		d.streak++
	} else {
		d.streak = 0
	}
	if d.streak >= 2 {
		d.hot = !d.hot
		d.streak = 0
	}
	return d.hot
}

// Guard is the running control loop. Construct with New and start Run in
// its own goroutine; it stops when ctx is done and performs no writes
// after that (same contract as the reconciler).
type Guard struct {
	cfg      Config
	client   kubernetes.Interface
	lister   corelisters.PodLister
	recorder *observe.Recorder
	logger   *slog.Logger

	dec decider

	// owned closes the informer-lag window between persisting the marker and
	// observing that PATCH back through the cache.
	owned map[string]ownedState

	// readTotalUsage and numCPU are injectable for tests.
	readTotalUsage func() (uint64, error)
	numCPU         func() int
	reportHealth   func(bool)

	prevTotal   uint64
	prevIdle    map[string]uint64 // pod UID -> usage_usec at previous tick
	prevSampled time.Time
}

// New builds a Guard. lister must be node-scoped (the agent's informer
// already is); client persists ownership markers and recorder pairs every
// cgroup change with an Event and metric increment.
func New(cfg Config, client kubernetes.Interface, lister corelisters.PodLister, recorder *observe.Recorder, logger *slog.Logger) *Guard {
	if client == nil {
		panic("guard: New: client must not be nil")
	}
	if lister == nil {
		panic("guard: New: lister must not be nil")
	}
	if recorder == nil {
		panic("guard: New: recorder must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	g := &Guard{
		cfg:      cfg,
		client:   client,
		lister:   lister,
		recorder: recorder,
		logger:   logger,
		owned:    make(map[string]ownedState),
		prevIdle: make(map[string]uint64),
	}
	g.readTotalUsage = func() (uint64, error) {
		return readUsageUsec(filepath.Join(cfg.CgroupRoot, "cpu.stat"))
	}
	g.numCPU = runtime.NumCPU
	g.reportHealth = func(bool) {}
	return g
}

// SetHealthReporter installs the readiness callback invoked after every
// enabled guard tick. It must be called before Run starts.
func (g *Guard) SetHealthReporter(report func(bool)) {
	if report == nil {
		g.reportHealth = func(bool) {}
		return
	}
	g.reportHealth = report
}

// Run executes the sampling loop until ctx is done. It deliberately performs
// no cleanup writes during shutdown: the operator-wide shutdown invariant is
// that SIGTERM never changes cgroup state. Lifecycle joins this method so no
// guard work remains after it returns; the next enabled process startup, or an
// explicit --revert-all, recovers any durable suppression marker.
func (g *Guard) Run(ctx context.Context) error {
	if !g.cfg.Enabled() {
		return nil
	}
	if g.cfg.Period <= 0 {
		return fmt.Errorf("guard: sampling period must be positive, got %s", g.cfg.Period)
	}
	ticker := time.NewTicker(g.cfg.Period)
	defer ticker.Stop()
	g.logger.Info("node guard started",
		"high", g.cfg.High, "low", g.cfg.Low,
		"period", g.cfg.Period.String(), "floor", g.cfg.FloorQuota)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			g.tick(ctx)
		}
	}
}

// tick takes one sample and converges every eligible pod's cpu.max to the
// value the current temperature calls for.
func (g *Guard) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	healthy := true
	defer func() { g.reportHealth(healthy) }()
	pods, err := g.lister.List(labels.Everything())
	if err != nil {
		healthy = false
		g.logger.Error("guard: list pods", "error", err)
		return
	}

	idlePods, idleUsage, idleErr := g.idlePodsUsage(pods)
	if idleErr != nil {
		healthy = false
		g.logger.Error("guard: inspect idle-tier pods", "error", idleErr)
		if convergeErr := g.converge(ctx, g.dec.hot, pods, idlePods); convergeErr != nil {
			g.logger.Error("guard: converge current state", "error", convergeErr)
		}
		g.prevSampled = time.Time{}
		g.prevIdle = make(map[string]uint64)
		return
	}

	// Eligibility cleanup must still run when node accounting is temporarily
	// unreadable; it must not depend on a successful cpu.stat sample.
	total, err := g.readTotalUsage()
	if err != nil {
		healthy = false
		g.logger.Error("guard: read node cpu.stat", "error", err)
		if convergeErr := g.converge(ctx, g.dec.hot, pods, idlePods); convergeErr != nil {
			g.logger.Error("guard: converge current state", "error", convergeErr)
		}
		g.prevSampled = time.Time{}
		g.prevIdle = make(map[string]uint64)
		return
	}

	now := time.Now()
	hot := g.dec.hot
	if !g.prevSampled.IsZero() && total >= g.prevTotal {
		elapsed := now.Sub(g.prevSampled).Microseconds()
		cpus := g.numCPU()
		idleDelta, complete := stableUsageDelta(g.prevIdle, idleUsage)
		if elapsed > 0 && cpus > 0 && complete {
			totalDelta := total - g.prevTotal
			// Per-pod files and the ancestor cpu.stat are not read
			// atomically. If their deltas contradict containment, treating
			// the negative remainder as zero would manufacture a cool
			// sample and could lift suppression under unknown pressure.
			if idleDelta <= totalDelta {
				nonIdleDelta := totalDelta - idleDelta
				util := float64(nonIdleDelta) / float64(elapsed) / float64(cpus)
				hot = g.dec.observe(util, g.cfg.High, g.cfg.Low)
				g.logger.Debug("guard tick", "non_idle_util", fmt.Sprintf("%.3f", util), "hot", hot)
			}
		}
	}
	// Converge exactly once, after the sample has selected this tick's final
	// temperature. Converging the old state first would briefly suppress a
	// newly eligible pod on the same tick that transitions to cool, only to
	// restore it immediately and emit two misleading outcomes.
	if err := g.converge(ctx, hot, pods, idlePods); err != nil {
		healthy = false
		g.logger.Error("guard: converge state", "error", err)
	}

	g.prevTotal = total
	g.prevIdle = idleUsage
	g.prevSampled = now
}

// stableUsageDelta returns an aggregate delta only when the sampled pod set
// is unchanged and every per-pod counter is monotonic. A newly created or
// removed idle pod has CPU time inside the node-wide interval but no matching
// endpoint in one of the per-pod snapshots; treating that incomplete amount
// as non-idle load can manufacture a hot transition during ordinary pod
// churn. Counter resets have the same ambiguity and also reset the baseline.
func stableUsageDelta(previous, current map[string]uint64) (uint64, bool) {
	if len(previous) != len(current) {
		return 0, false
	}
	var total uint64
	for uid, usage := range current {
		old, ok := previous[uid]
		if !ok || usage < old {
			return 0, false
		}
		delta := usage - old
		if delta > math.MaxUint64-total {
			return 0, false
		}
		total += delta
	}
	return total, true
}

// idlePodsUsage returns running, requested idle-tier pods whose live
// cpu.idle is active, plus their current cpu.stat usage keyed by UID. The
// live knob check prevents a failed or delayed tier write from making an
// ordinary workload disappear from non-idle utilization or become a guard
// suppression target. Actual cpu.max, not the Pod spec, then decides which
// candidates converge may suppress: kubelet can intentionally leave a spec
// limit unenforced, or not have applied it yet.
func (g *Guard) idlePodsUsage(pods []*corev1.Pod) ([]*corev1.Pod, map[string]uint64, error) {
	var candidates []*corev1.Pod
	usage := make(map[string]uint64)
	var errs []error
	for _, pod := range pods {
		if pod.Annotations[annotations.TierKey] != annotations.TierValueIdle {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		dir, err := cgroup.PodCgroupPath(g.cfg.CgroupRoot, g.cfg.KubepodsName, g.cfg.Driver, qos.ToCgroupClass(qos.ClassOf(pod.Spec)), string(pod.UID))
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s/%s cgroup path: %w", pod.Namespace, pod.Name, err))
			continue
		}
		idle, err := cgroup.ReadKnob(dir, "cpu.idle")
		if err != nil {
			if !errors.Is(err, cgroup.ErrCgroupGone) {
				errs = append(errs, fmt.Errorf("pod %s/%s read cpu.idle: %w", pod.Namespace, pod.Name, err))
			}
			continue
		}
		if idle != "0" && idle != "1" {
			errs = append(errs, fmt.Errorf("pod %s/%s has invalid cpu.idle %q", pod.Namespace, pod.Name, idle))
			continue
		}
		if idle != "1" {
			continue
		}
		if u, usageErr := readUsageUsec(filepath.Join(dir, "cpu.stat")); usageErr == nil {
			usage[string(pod.UID)] = u
		} else if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			if statErr == nil {
				errs = append(errs, fmt.Errorf("pod %s/%s read cpu.stat: %w", pod.Namespace, pod.Name, usageErr))
			} else {
				errs = append(errs, fmt.Errorf("pod %s/%s read cpu.stat: %w (verify cgroup: %v)", pod.Namespace, pod.Name, usageErr, statErr))
			}
		}
		// Never introduce or retain pressure throttling while kubelet is
		// trying to terminate the workload. Its usage still belongs in the
		// idle subtraction above until the cgroup disappears, but it is not a
		// suppression candidate; converge will restore an owned one.
		if pod.DeletionTimestamp != nil {
			continue
		}
		candidates = append(candidates, pod)
	}
	return candidates, usage, errors.Join(errs...)
}

// readUsageUsec extracts usage_usec from a cgroup v2 cpu.stat file.
func readUsageUsec(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "usage_usec "); ok {
			return strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		}
	}
	return 0, fmt.Errorf("guard: no usage_usec in %s", path)
}
