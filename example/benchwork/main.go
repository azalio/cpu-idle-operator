// benchwork is the load target for the example scenario: an HTTP server
// whose /work endpoint burns a fixed, deterministic amount of CPU per
// request and nothing else — no I/O, no allocations in the hot loop, no
// shared state between requests. That makes its latency a clean probe of
// one thing only: how much CPU the pod's cgroup is actually getting.
//
// BENCH_ITERATIONS (default 1_000_000) sets the per-request work. On the
// measured stand one million xorshift iterations cost single-digit
// milliseconds of CPU; scale it to your cores so that the baseline p99
// sits comfortably inside the SLO.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	completed atomic.Uint64
	sink      atomic.Uint64
)

type stats struct {
	Completed  uint64 `json:"completed"`
	Iterations int    `json:"iterations_per_request"`
	GOMAXPROCS int    `json:"gomaxprocs"`
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}

// requestWork is deterministic CPU work with a periodic cancellation
// check, so a client that has already timed out does not keep burning
// the server's CPU. The result is published to a package-level sink so
// the compiler cannot delete the loop.
func requestWork(ctx context.Context, iterations int, seed uint64) (uint64, bool) {
	x := seed | 1
	for i := 0; i < iterations; i++ {
		if i&0x3fff == 0 {
			select {
			case <-ctx.Done():
				return 0, false
			default:
			}
		}
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		x *= 0x9e3779b185ebca87
	}
	return x, true
}

func main() {
	iterations := envInt("BENCH_ITERATIONS", 1_000_000)
	listen := os.Getenv("BENCH_LISTEN")
	if listen == "" {
		listen = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats{
			Completed:  completed.Load(),
			Iterations: iterations,
			GOMAXPROCS: runtime.GOMAXPROCS(0),
		})
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestID := completed.Add(1)
		result, finished := requestWork(r.Context(), iterations, requestID)
		if !finished {
			return
		}
		sink.Store(result)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "ok %016x\n", result)
	})

	server := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("benchwork listen=%s iterations=%d gomaxprocs=%d",
		listen, iterations, runtime.GOMAXPROCS(0))
	log.Fatal(server.ListenAndServe())
}
