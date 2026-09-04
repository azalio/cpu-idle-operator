package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompletedCountsOnlyFinishedWork(t *testing.T) {
	completed.Store(0)
	sequence.Store(0)
	sink.Store(0)
	handler := newHandler(10_000)

	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := httptest.NewRequest(http.MethodGet, "/work", nil).WithContext(cancelledContext)
	handler.ServeHTTP(httptest.NewRecorder(), cancelled)
	if got := completed.Load(); got != 0 {
		t.Fatalf("completed after cancelled request = %d, want 0", got)
	}

	success := httptest.NewRequest(http.MethodGet, "/work", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, success)
	if response.Code != http.StatusOK {
		t.Fatalf("successful response status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := completed.Load(); got != 1 {
		t.Fatalf("completed after one finished request = %d, want 1", got)
	}
}
