package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthTracksPersistentReconcileFailures(t *testing.T) {
	health := NewHealth()
	health.SetGateResult(true, "ok")
	health.SetSynced(true)
	if ready, reason := health.Ready(); !ready || reason != "" {
		t.Fatalf("initial Ready() = (%v, %q), want (true, empty)", ready, reason)
	}

	health.SetReconcileHealthy(false)
	if ready, reason := health.Ready(); ready || reason != reasonReconcileFailed {
		t.Fatalf("Ready() after failure = (%v, %q), want (false, %q)", ready, reason, reasonReconcileFailed)
	}
	response := httptest.NewRecorder()
	health.ReadinessHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), reasonReconcileFailed) {
		t.Fatalf("/readyz = %d %q, want 503 containing %q", response.Code, response.Body.String(), reasonReconcileFailed)
	}

	health.SetReconcileHealthy(true)
	if ready, reason := health.Ready(); !ready || reason != "" {
		t.Fatalf("Ready() after recovery = (%v, %q), want (true, empty)", ready, reason)
	}
}

func TestHealthTracksGuardFailures(t *testing.T) {
	health := NewHealth()
	health.SetGateResult(true, "ok")
	health.SetSynced(true)

	health.SetGuardHealthy(false)
	if ready, reason := health.Ready(); ready || reason != reasonGuardFailed {
		t.Fatalf("Ready() after guard failure = (%v, %q), want (false, %q)", ready, reason, reasonGuardFailed)
	}

	health.SetGuardHealthy(true)
	if ready, reason := health.Ready(); !ready || reason != "" {
		t.Fatalf("Ready() after guard recovery = (%v, %q), want (true, empty)", ready, reason)
	}
}
