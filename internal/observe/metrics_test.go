package observe

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestVC1MetricFamiliesPresent scrapes a fresh registry after NewMetrics and
// after each family has recorded at least one sample. A GaugeVec or
// CounterVec with no label values set yet produces no series at all (a
// known prometheus/client_golang behavior), so a meaningful presence check
// has to touch every family once first, the way real call sites eventually
// will.
func TestVC1MetricFamiliesPresent(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.PodsInTier.WithLabelValues("node-a", "default", "Burstable", "idle").Set(1)
	metrics.TierApplyTotal.WithLabelValues("node-a", "default", "Burstable", "applied", "ok").Inc()
	metrics.ResyncDriftTotal.WithLabelValues("node-a", "default", "Burstable").Inc()
	metrics.EnvironmentGateInfo.WithLabelValues("node-a", "kernel_too_old").Set(1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	want := map[string]bool{
		"cpu_pods_in_tier":          false,
		"cpu_tier_apply_total":      false,
		"cpu_resync_drift_total":    false,
		"cpu_environment_gate_info": false,
	}
	for _, family := range families {
		if _, ok := want[family.GetName()]; ok {
			want[family.GetName()] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("metric family %q missing from Gather() output", name)
		}
	}
}

// TestVC2ForbiddenLabelsRejected checks HC-5 two ways: the low-level
// constructor every metric in this package goes through rejects each of the
// three forbidden label names outright (the "what gets written later"
// half), and — independently — every label actually registered by
// NewMetrics today is within the allowlist (the "what is already written"
// half), by walking a real Gather() snapshot rather than trusting the
// source code by inspection.
func TestVC2ForbiddenLabelsRejected(t *testing.T) {
	forbidden := []string{"pod_name", "pod_uid", "container_id"}
	for _, label := range forbidden {
		t.Run(label, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("newGaugeVec accepted forbidden label %q without panicking", label)
				}
			}()
			newGaugeVec(prometheus.GaugeOpts{Name: "test_forbidden_metric"}, []string{"node", label})
		})
	}

	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.PodsInTier.WithLabelValues("node-a", "default", "Burstable", "idle").Set(1)
	metrics.TierApplyTotal.WithLabelValues("node-a", "default", "Burstable", "applied", "ok").Inc()
	metrics.ResyncDriftTotal.WithLabelValues("node-a", "default", "Burstable").Inc()
	metrics.EnvironmentGateInfo.WithLabelValues("node-a", "kernel_too_old").Set(1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, labelPair := range metric.GetLabel() {
				if !allowedMetricLabelSet[labelPair.GetName()] {
					t.Errorf("metric %s carries forbidden label %q", family.GetName(), labelPair.GetName())
				}
			}
		}
	}
}
