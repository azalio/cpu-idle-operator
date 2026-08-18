package agent

import (
	"fmt"
	"net/http"
	"sync"
)

// reasonCacheNotSynced is Health's readiness reason when the environment
// gate passed but the informer's cache has not completed its initial sync
// yet. It deliberately does not reuse any envgate.Reason value: this
// condition has nothing to do with the environment gate's own decision.
const reasonCacheNotSynced = "cache_not_synced"

// Health tracks this agent process's combined readiness decision and
// serves it over HTTP. Readiness requires two conditions to both hold
// (VC4): the environment gate passed, and the informer's cache has
// completed its initial sync. A caller that reported 200 before both held
// would tell Kubernetes this pod can already reconcile pods it has not
// even listed yet — worse, on a failed gate, it would claim the node is
// fine when INV-5 says the opposite. The gate's own outcome is set once at
// startup and never changes for this process's lifetime; the sync flag
// starts false and is set at most once, when the informer's cache finishes
// its initial list.
type Health struct {
	mu         sync.RWMutex
	gateReady  bool
	gateReason string
	synced     bool
}

// NewHealth returns a Health that reports not-ready until both
// SetGateResult(true, ...) and SetSynced(true) have been called.
func NewHealth() *Health {
	return &Health{gateReason: reasonCacheNotSynced}
}

// SetGateResult records the environment gate's decision (envgate.Check):
// ready is Result.Ready and reason is Result.Reason's string form. Callers
// set this exactly once, right after the gate check runs.
func (h *Health) SetGateResult(ready bool, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gateReady = ready
	h.gateReason = reason
}

// SetSynced records whether the informer's cache has completed its initial
// sync.
func (h *Health) SetSynced(synced bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.synced = synced
}

// Ready reports the current combined readiness decision and, when not
// ready, the reason to surface to a caller. The gate's reason takes
// priority over "cache not synced yet": a failed gate is the longer-lived,
// more actionable condition, and VC1 requires the gate's own reason text
// (e.g. "cgroup_v1") to reach the readiness response body even before any
// informer has had a chance to sync.
func (h *Health) Ready() (ready bool, reason string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.gateReady {
		return false, h.gateReason
	}
	if !h.synced {
		return false, reasonCacheNotSynced
	}
	return true, ""
}

// ReadinessHandler serves /readyz: 200 "ok" once Ready reports true, 503
// with the failure reason in the response body otherwise (VC1: the body
// must contain the gate's reason text).
func (h *Health) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ready, reason := h.Ready()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			// Intent: a write error here only means the client already
			// disconnected after the status code was sent; the probe
			// result is what matters, and there is nothing left to react
			// to once WriteHeader has already gone out.
			_, _ = fmt.Fprintf(w, "not ready: %s\n", reason)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	}
}

// LivenessHandler serves /healthz: 200 whenever the process is up enough to
// serve HTTP at all. It is unconditional by design (AC-6): a failed
// environment gate must never make this process look unhealthy to
// kubelet's liveness probe, or a restart would follow and crash-loop the
// pod on every gate-failing node in the cluster. The gate's failure is
// reported through readiness, never through liveness.
func (h *Health) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Intent: same reasoning as ReadinessHandler above -- a write error
		// here only means the client disconnected after the status code
		// was already sent.
		_, _ = fmt.Fprintln(w, "ok")
	}
}

// NewServer builds an *http.Server serving this Health's liveness endpoint
// at /healthz and readiness endpoint at /readyz. addr is informational only
// when the caller drives the server through a pre-built net.Listener (as
// Lifecycle.Run does); it still sets http.Server.Addr for callers that use
// ListenAndServe directly.
func (h *Health) NewServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.LivenessHandler())
	mux.HandleFunc("/readyz", h.ReadinessHandler())
	return &http.Server{Addr: addr, Handler: mux}
}
