package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/azalio/cpu-idle-operator/internal/apply"
	"github.com/azalio/cpu-idle-operator/internal/config"
	"github.com/azalio/cpu-idle-operator/internal/envgate"
	"github.com/azalio/cpu-idle-operator/internal/guard"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/tier"
)

// componentName identifies this agent as the source of the Kubernetes
// Events it raises (EventSource.Component).
const componentName = "cpu-idle-agent"

// shutdownTimeout bounds how long Run waits for the metrics and health
// HTTP servers to finish in-flight requests once shutdown begins. It only
// gates the HTTP servers: the reconcile and guard loops are cancelled and
// joined separately.
const shutdownTimeout = 10 * time.Second

// GateCheckFunc matches envgate.Check's signature. Lifecycle depends on it
// through this indirection rather than calling envgate.Check directly, so
// a test can inject the environment gate's decision without needing a real
// (or realistically faked) cgroup v2 mount and kernel: envgate.Check's own
// decision logic is already covered by internal/envgate/gate_test.go, and
// that package's Check exposes no seam a caller outside it can use to fake
// a passing cgroup v2 mount (its statfs-mocking hook is an unexported
// package variable). This package's own tests only need to prove what
// Lifecycle does with a given decision, not re-derive the decision itself.
type GateCheckFunc func(root, kubepodsName string, uname envgate.UnameFunc) (envgate.Result, error)

// Lifecycle owns cmd/agent's full startup and shutdown sequence: flag
// parsing happens in the caller (config.ParseFlags), but everything after
// that — the environment gate decision, the metrics/health HTTP servers,
// and the branch between the full reconcile loop and read-only degraded
// mode — lives here, so cmd/agent/main.go stays a thin wrapper and this
// behavior is testable without a real cluster or a real kernel underneath
// it (see cmd/agent/main_test.go and lifecycle_test.go).
//
// Run never changes cgroup state during shutdown (INV-4), including transient
// node-guard suppression. It cancels and joins the guard so no writes remain in
// flight. A later process with the guard still enabled recovers the durable
// marker before becoming ready; explicit --revert-all provides cleanup before
// disabling the guard or permanently removing the operator. A disabled guard
// never acts on Pod metadata, which is tenant-controlled.
type Lifecycle struct {
	// Client is this agent's Kubernetes API client. Required.
	Client kubernetes.Interface
	// Config is the agent's parsed flags (config.ParseFlags's result).
	Config config.Config
	// Logger receives this Lifecycle's structured log lines. Defaults to
	// slog.Default() when nil.
	Logger *slog.Logger
	// Uname is the environment gate's kernel-release source, passed
	// through to GateCheck. Defaults to the real uname(2) syscall when
	// nil; tests pin a release string instead.
	Uname envgate.UnameFunc
	// GateCheck decides the environment gate's outcome. Defaults to
	// envgate.Check when nil; see GateCheckFunc's doc comment for why
	// tests override this instead of faking a cgroup v2 mount.
	GateCheck GateCheckFunc
	// EventRecorder is the client-go recorder Kubernetes Events flow
	// through, for both the degraded read-only branch's
	// EnvironmentUnsupported Events and the ready branch's
	// observe.Recorder/observe.EventRecorder wiring. Defaults to a
	// broadcaster wired to Client's event sink when nil; tests inject a
	// record.NewFakeRecorder to assert on emitted Events without a real
	// API server.
	EventRecorder record.EventRecorder
	// Applier is the Reconciler dependency the ready branch calls for
	// every actual convergence. Defaults to a real *apply.Applier (via
	// apply.NewApplier) when nil — this is the only construction site for
	// one anywhere in this Lifecycle, and it only runs inside the gate's
	// Ready branch, which is what makes INV-5 ("applier == nil when the
	// gate failed") hold by construction rather than by a runtime check.
	// Tests substitute a call-journaling fake to prove the shutdown path
	// invokes it zero times (VC3) without touching a filesystem.
	Applier Applier
	// MetricsListener, when set, is used instead of listening on
	// Config.MetricsAddr. Tests bind "127.0.0.1:0" themselves so they can
	// read back the actual ephemeral port before calling Run.
	MetricsListener net.Listener
	// HealthListener is MetricsListener's counterpart for Config.HealthAddr.
	HealthListener net.Listener

	// Health is populated once Run starts. Tests may read it directly
	// instead of going through the HTTP endpoint, though health.go's
	// endpoints are the actual contract under test for VC1/VC4.
	Health *Health
}

// Run executes this agent's full lifecycle: the environment gate check,
// the metrics and health HTTP servers, and then either the full reconcile
// loop (gate passed) or read-only degraded mode (gate failed). It blocks
// until ctx is done or any essential loop/server exits, then cancels and
// joins every other component before returning. See the type doc comment
// for Run's INV-4 shutdown guarantee.
func (lc *Lifecycle) Run(ctx context.Context) error {
	logger := lc.logger()

	if lc.Client == nil {
		return errors.New("agent: Lifecycle.Run: Client must not be nil")
	}
	if lc.Config.NodeName == "" {
		return errors.New("agent: Lifecycle.Run: Config.NodeName must not be empty")
	}

	gateResult, err := lc.gateCheck()(lc.Config.CgroupRoot, lc.Config.KubepodsName, lc.uname())
	if err != nil {
		return fmt.Errorf("agent: environment gate check: %w", err)
	}

	registry := prometheus.NewRegistry()
	metrics := observe.NewMetrics(registry)
	gatherers := prometheus.Gatherers{registry}

	health := NewHealth()
	lc.Health = health
	health.SetGateResult(gateResult.Ready, string(gateResult.Reason))

	eventRecorder, stopEventRecorder := lc.eventRecorder()
	defer stopEventRecorder()

	// Intent: observe.NewRecorderFromMetrics reuses the single Metrics
	// bundle already registered on registry above instead of registering a
	// second bundle on a second registry (the seam that used to be
	// missing, per observe.NewRecorder's own doc comment) — this process
	// now exposes exactly one Prometheus registry, so there is no second
	// registry a future metric could collide with and take down the whole
	// /metrics scrape (Gather() fails atomically across every family the
	// instant two collide, not just the colliding one).
	var applier Applier
	var recorder *observe.Recorder
	if gateResult.Ready {
		recorder = observe.NewRecorderFromMetrics(metrics, eventRecorder, lc.Config.NodeName)

		applier = lc.Applier
		if applier == nil {
			applier = apply.NewApplier(lc.Config.CgroupRoot, lc.Config.KubepodsName, gateResult.Driver, recorder, observe.NewEventRecorder(eventRecorder))
		}
	} else {
		// Intent: EnvironmentGateInfo's own doc comment says the series is
		// absent once the node passes, so this Set call must be the only
		// place in this Lifecycle that ever touches it.
		metrics.EnvironmentGateInfo.WithLabelValues(lc.Config.NodeName, string(gateResult.Reason)).Set(1)
		logger.Error("environment gate failed; agent stays alive in read-only mode",
			"reason", string(gateResult.Reason))
	}

	// Intent: build the informer before either HTTP listener is bound, so a
	// handler-registration failure returns before any server has started --
	// no shutdown/cleanup path is needed for servers that were never
	// brought up in the first place.
	informer, err := NewInformer(lc.Client, lc.Config.NodeName, lc.Config.ResyncPeriod)
	if err != nil {
		return fmt.Errorf("agent: informer: %w", err)
	}
	informer.logger = logger
	informer.onReconcileHealth = health.SetReconcileHealthy

	metricsListener, err := lc.listener(lc.MetricsListener, lc.Config.MetricsAddr)
	if err != nil {
		return fmt.Errorf("agent: metrics listener: %w", err)
	}
	healthListener, err := lc.listener(lc.HealthListener, lc.Config.HealthAddr)
	if err != nil {
		_ = metricsListener.Close()
		return fmt.Errorf("agent: health listener: %w", err)
	}

	metricsServer := newMetricsServer(gatherers)
	healthServer := health.NewServer(lc.Config.HealthAddr)

	var serverWG sync.WaitGroup
	serverErrs := make(chan error, 2)
	lc.serve(&serverWG, serverErrs, metricsServer, metricsListener, "metrics")
	lc.serve(&serverWG, serverErrs, healthServer, healthListener, "health")

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	runErrs := make(chan error, 1)
	go func() {
		if gateResult.Ready {
			reconciler := NewReconciler(informer.Lister(), applier, lc.Config.CgroupRoot, lc.Config.KubepodsName, gateResult.Driver, metrics, lc.Config.NodeName)
			guardCfg := guard.Config{
				High:         lc.Config.GuardHigh,
				Low:          lc.Config.GuardLow,
				Period:       lc.Config.GuardPeriod,
				FloorQuota:   lc.Config.GuardFloor,
				CgroupRoot:   lc.Config.CgroupRoot,
				KubepodsName: lc.Config.KubepodsName,
				Driver:       gateResult.Driver,
				NodeName:     lc.Config.NodeName,
			}
			nodeGuard := guard.New(guardCfg, lc.Client, informer.Lister(), recorder, logger)
			nodeGuard.SetHealthReporter(health.SetGuardHealthy)
			runErrs <- lc.runReady(runCtx, informer, reconciler, health, nodeGuard)
			return
		}
		runErrs <- lc.runDegraded(runCtx, informer, observe.NewEventRecorder(eventRecorder))
	}()

	// Either the control loop or either HTTP server is essential. If one
	// exits unexpectedly, cancel and join the control loop immediately so
	// the process can fail and be restarted instead of staying alive with a
	// dead metrics, readiness, or reconciliation surface.
	var runErr error
	select {
	case runErr = <-runErrs:
	case serveErr := <-serverErrs:
		runErr = serveErr
		cancelRun()
		if workerErr := <-runErrs; workerErr != nil && !errors.Is(workerErr, context.Canceled) {
			runErr = errors.Join(runErr, workerErr)
		}
	}
	cancelRun()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if shutdownErr := metricsServer.Shutdown(shutdownCtx); shutdownErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("agent: metrics server shutdown: %w", shutdownErr))
	}
	if shutdownErr := healthServer.Shutdown(shutdownCtx); shutdownErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("agent: health server shutdown: %w", shutdownErr))
	}
	serverWG.Wait()
	close(serverErrs)
	for serveErr := range serverErrs {
		runErr = errors.Join(runErr, serveErr)
	}

	return runErr
}

// runReady drives the gate-passed branch: it waits for informer's cache to
// perform its initial sync — an idempotent full-node reconciliation, since
// every key the initial List delivers is queued exactly like a live event
// — recovers guard-owned state when the guard is enabled, marks Health
// synced once startup recovery completes (VC4), then blocks draining the
// workqueue through reconciler.Reconcile until ctx is done. A disabled guard
// skips marker recovery because Pod annotations are tenant-controlled; the
// explicit --revert-all path remains available for deliberate cleanup.
func (lc *Lifecycle) runReady(ctx context.Context, informer *Informer, reconciler *Reconciler, health *Health, nodeGuard *guard.Guard) error {
	if !informer.Start(ctx) {
		// Intent: Start's own doc comment says it returns false only when
		// ctx was done before the cache finished syncing — i.e. this path
		// is always a shutdown racing startup, never a distinct failure
		// mode. Returning nil here matches Informer.Run's own convention
		// of a nil-returning shutdown on ctx cancellation, so an unlucky
		// SIGTERM during startup does not make main.go exit non-zero.
		return nil
	}
	if nodeGuard.Enabled() {
		if err := nodeGuard.Recover(ctx); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return nil
			}
			return fmt.Errorf("agent: recover node guard state: %w", err)
		}
	}
	health.SetSynced(true)

	if !nodeGuard.Enabled() {
		return informer.Run(ctx, reconciler.Reconcile)
	}

	loopCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()
	type loopResult struct {
		name string
		err  error
	}
	loopResults := make(chan loopResult, 2)
	go func() {
		loopResults <- loopResult{name: "guard", err: nodeGuard.Run(loopCtx)}
	}()
	go func() {
		loopResults <- loopResult{name: "informer", err: informer.Run(loopCtx, reconciler.Reconcile)}
	}()

	first := <-loopResults
	cancelLoops()
	second := <-loopResults
	var loopErrs []error
	for _, result := range []loopResult{first, second} {
		if result.err != nil {
			loopErrs = append(loopErrs, fmt.Errorf("agent: %s loop: %w", result.name, result.err))
		}
	}
	return errors.Join(loopErrs...)
}

// runDegraded drives the gate-failed branch (INV-5: no Applier exists on
// this call path at all, not even as a nil-checked reference). It runs the
// same Informer/workqueue machinery as the ready branch, but its
// "reconcile" function never touches a cgroup: for every pod that requests
// a tier this node cannot honor, it raises exactly one
// EnvironmentUnsupported Event and never repeats it for that pod's UID —
// without the dedup, the informer's periodic full resync would replay the
// same Add-shaped delivery every resync period and spam the same Event
// forever. A key-to-UID index removes dedup state on Delete/recreate in O(1)
// per event; this degraded path never scans the full informer cache once per
// queued Pod.
func (lc *Lifecycle) runDegraded(ctx context.Context, informer *Informer, events *observe.EventRecorder) error {
	notified := make(map[types.UID]string)
	uidsByKey := make(map[string]types.UID)

	reconcile := func(_ context.Context, key string, _ bool) error {
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			return fmt.Errorf("agent: degraded: split key %q: %w", key, err)
		}
		pod, err := informer.Lister().Pods(namespace).Get(name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				if uid, ok := uidsByKey[key]; ok {
					delete(notified, uid)
					delete(uidsByKey, key)
				}
				return nil
			}
			return fmt.Errorf("agent: degraded: get pod %s: %w", key, err)
		}
		if previousUID, ok := uidsByKey[key]; ok && previousUID != pod.UID {
			delete(notified, previousUID)
		}
		uidsByKey[key] = pod.UID

		desired, notes := tier.Desired(pod)
		unsupported := desired.IdleRequested || desired.BurstRequested
		signature := noteSignature(notes)
		if unsupported {
			signature += string(observe.TierApplyReasonEnvironmentUnsupported) + "\x00"
		}

		previous, seen := notified[pod.UID]
		if signature == "" {
			delete(notified, pod.UID)
		} else {
			notified[pod.UID] = signature
		}
		if signature == "" || (seen && previous == signature) {
			return nil
		}

		for _, note := range notes {
			if note.Code == tier.NoteUnknownTierValue {
				events.TierValueUnknown(pod, "%s", note.Message)
			}
		}
		if unsupported {
			events.EnvironmentUnsupported(pod,
				"node %s does not support cpu-idle-operator cgroup controls; tier annotations on this pod cannot take effect",
				lc.Config.NodeName)
		}
		return nil
	}

	return informer.Run(ctx, reconcile)
}

// gateCheck returns lc.GateCheck, defaulting to envgate.Check.
func (lc *Lifecycle) gateCheck() GateCheckFunc {
	if lc.GateCheck != nil {
		return lc.GateCheck
	}
	return envgate.Check
}

// uname returns lc.Uname, defaulting to the real uname(2) syscall.
func (lc *Lifecycle) uname() envgate.UnameFunc {
	if lc.Uname != nil {
		return lc.Uname
	}
	return realUname
}

// logger returns lc.Logger, defaulting to slog.Default().
func (lc *Lifecycle) logger() *slog.Logger {
	if lc.Logger != nil {
		return lc.Logger
	}
	return slog.Default()
}

// eventRecorder returns lc.EventRecorder, defaulting to a broadcaster
// wired to Client's event sink — production's real path, since this
// operator's Events belong on the API server, not only in this process's
// own logs. The returned stop function joins the default broadcaster; it
// is a no-op for an injected recorder owned by the caller.
func (lc *Lifecycle) eventRecorder() (record.EventRecorder, func()) {
	if lc.EventRecorder != nil {
		return lc.EventRecorder, func() {}
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: lc.Client.CoreV1().Events("")})
	return broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: componentName, Host: lc.Config.NodeName}), broadcaster.Shutdown
}

// listener returns injected unchanged, or listens on addr otherwise.
// Listening synchronously here — instead of inside the server goroutine —
// makes a bind failure (e.g. the port already in use) surface as a Run
// error immediately, rather than silently in the background after Run has
// already moved on to the reconcile loop.
func (lc *Lifecycle) listener(injected net.Listener, addr string) (net.Listener, error) {
	if injected != nil {
		return injected, nil
	}
	return net.Listen("tcp", addr)
}

// serve runs server.Serve(listener) in a background goroutine tracked by
// wg, reporting any error other than http.ErrServerClosed (the expected
// error on a graceful Shutdown) on errs.
func (lc *Lifecycle) serve(wg *sync.WaitGroup, errs chan<- error, server *http.Server, listener net.Listener, name string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("agent: %s server: %w", name, err)
		}
	}()
}

// newMetricsServer builds the /metrics HTTP server from the process's
// single registry (wrapped as Gatherers for promhttp's Gatherer API).
func newMetricsServer(gatherers prometheus.Gatherers) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{}))
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}
