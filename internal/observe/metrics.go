// Package observe implements this operator's Prometheus metrics and
// Kubernetes Event recording (AC-8, CCR-1). metrics.go registers every
// metric family on a caller-supplied prometheus.Registry rather than the
// global default registry, so tests (and a future scrape handler) can
// Gather a clean snapshot with no cross-test or global state.
//
// Every label name any metric in this file uses is checked against
// allowedMetricLabelNames before the metric is even constructed — not only
// by metrics_test.go — because HC-5 forbids pod_name, pod_uid, and
// container_id. A label added to a metric definition later must fail the
// moment this code runs at startup, not only when a human remembers to
// update the test: pod_name looked like a bounded value during the initial
// research pass and was not — a Deployment pod's name carries a random
// suffix that changes on every restart, the same cardinality blow-up as the
// pod_uid HC-5 already names outright (see resolution T-003, "Правки после
// ревью", point 2).
package observe

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// allowedMetricLabelNames is the exhaustive HC-5 allowlist: no metric this
// package registers may carry a label outside this set.
var allowedMetricLabelNames = []string{"node", "namespace", "qos_class", "tier", "result", "reason"}

// allowedMetricLabelSet is allowedMetricLabelNames as a set, built once at
// package init for an O(1) membership check.
var allowedMetricLabelSet = func() map[string]bool {
	set := make(map[string]bool, len(allowedMetricLabelNames))
	for _, name := range allowedMetricLabelNames {
		set[name] = true
	}
	return set
}()

// Metrics is the fixed set of Prometheus metric families this operator
// exports (AC-8): pod counts per tier, tier-apply outcomes, resync drift,
// and the environment-gate decision. NewMetrics registers all four; this
// package never writes to the global default registry.
type Metrics struct {
	// PodsInTier is the number of pods currently in each CPU tier on this
	// node. Reconciler is the only writer: one full live-cgroup scan seeds
	// the series, then serialized per-pod reconciliation updates membership
	// incrementally (see Reconciler.refreshPodsInTier).
	PodsInTier *prometheus.GaugeVec
	// TierApplyTotal counts tier-apply attempts, split by outcome and
	// reason. Recorder is the only intended caller: it increments this
	// together with the paired Event so CCR-1 cannot be satisfied halfway.
	TierApplyTotal *prometheus.CounterVec
	// ResyncDriftTotal counts resync passes that found actual cgroup state
	// disagreeing with the desired state from annotations (resolution
	// T-011): a sustained non-zero rate here means an unexpected writer is
	// present.
	ResyncDriftTotal *prometheus.CounterVec
	// EnvironmentGateInfo is 1 while this node fails the environment gate,
	// labeled with the failure reason (resolution T-009): the node stays
	// visible on a dashboard, not only in logs.
	EnvironmentGateInfo *prometheus.GaugeVec
}

// NewMetrics builds this operator's metric families and registers them on
// registry. It panics if registry is nil, or if any metric it registers
// would carry a label name outside allowedMetricLabelNames — HC-5 must be
// enforced before a forbidden label ever reaches a scrape endpoint, not
// only when a test happens to catch it.
func NewMetrics(registry *prometheus.Registry) *Metrics {
	if registry == nil {
		panic("observe: NewMetrics: registry must not be nil")
	}

	m := &Metrics{
		PodsInTier: newGaugeVec(prometheus.GaugeOpts{
			Name: "cpu_pods_in_tier",
			Help: "Number of pods currently in each CPU tier on this node.",
		}, []string{"node", "namespace", "qos_class", "tier"}),

		TierApplyTotal: newCounterVec(prometheus.CounterOpts{
			Name: "cpu_tier_apply_total",
			Help: "Count of CPU-tier apply attempts, by outcome (result) and reason.",
		}, []string{"node", "namespace", "qos_class", "result", "reason"}),

		ResyncDriftTotal: newCounterVec(prometheus.CounterOpts{
			Name: "cpu_resync_drift_total",
			Help: "Count of resync passes where actual cgroup state disagreed with the desired state from annotations.",
		}, []string{"node", "namespace", "qos_class"}),

		EnvironmentGateInfo: newGaugeVec(prometheus.GaugeOpts{
			Name: "cpu_environment_gate_info",
			Help: "1 while this node fails the environment gate, labeled with the reason; the series is absent once the node passes.",
		}, []string{"node", "reason"}),
	}

	registry.MustRegister(m.PodsInTier, m.TierApplyTotal, m.ResyncDriftTotal, m.EnvironmentGateInfo)
	return m
}

// newGaugeVec and newCounterVec are the only two functions in this package
// that may call prometheus.NewGaugeVec/NewCounterVec directly: every metric
// this package defines, now or later, must go through one of them so the
// HC-5 label check actually runs at construction time (VC2), not only in a
// test that walks whatever got registered.

func newGaugeVec(opts prometheus.GaugeOpts, labelNames []string) *prometheus.GaugeVec {
	mustAllowedLabels(opts.Name, labelNames)
	return prometheus.NewGaugeVec(opts, labelNames)
}

func newCounterVec(opts prometheus.CounterOpts, labelNames []string) *prometheus.CounterVec {
	mustAllowedLabels(opts.Name, labelNames)
	return prometheus.NewCounterVec(opts, labelNames)
}

// mustAllowedLabels panics if any of labelNames falls outside
// allowedMetricLabelSet. metricName only makes the panic message point at
// the offending metric.
func mustAllowedLabels(metricName string, labelNames []string) {
	for _, name := range labelNames {
		if !allowedMetricLabelSet[name] {
			panic(fmt.Sprintf(
				"observe: metric %q declares forbidden label %q; only %v are allowed (HC-5)",
				metricName, name, allowedMetricLabelNames,
			))
		}
	}
}
