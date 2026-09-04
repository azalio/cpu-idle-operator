package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
)

// TestVC4FieldSelectorScopesToNode covers VC4: the informer's List call
// against the API server must carry spec.nodeName=<node> as a
// server-side field selector, not an in-memory filter -- the distinction
// this subtask's own risk list calls out (an in-memory filter would pull
// every pod in the cluster through this agent's watch, defeating the
// single-node RBAC scope resolution T-008 promises). The selector is
// observed by intercepting the fake clientset's "list" reactor, the only
// way to see what ListOptions a caller actually sent.
func TestVC4FieldSelectorScopesToNode(t *testing.T) {
	t.Run("test_vc4_field_selector_scopes_to_node", func(t *testing.T) {
		client := fake.NewSimpleClientset()

		var (
			mu       sync.Mutex
			captured string
			observed bool
		)
		client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if listAction, ok := action.(k8stesting.ListActionImpl); ok {
				mu.Lock()
				captured = listAction.GetListRestrictions().Fields.String()
				observed = true
				mu.Unlock()
			}
			// Intent: only observe the call, never replace the fixture's
			// own response -- returning handled=false lets the default
			// reactor chain (backed by the object tracker) answer as
			// usual.
			return false, nil, nil
		})

		informer, err := NewInformer(client, "node-a", time.Minute)
		if err != nil {
			t.Fatalf("NewInformer() error = %v, want nil", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if !informer.Start(ctx) {
			t.Fatal("informer cache did not sync within the test timeout")
		}

		mu.Lock()
		defer mu.Unlock()
		if !observed {
			t.Fatal("no list action was observed against the fake clientset")
		}
		if want := "spec.nodeName=node-a"; captured != want {
			t.Errorf("List() FieldSelector = %q, want %q", captured, want)
		}
	})
}

func TestProcessNextItemLogsReconcileFailureBeforeRequeue(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue[reconcileRequest](workqueue.DefaultTypedControllerRateLimiter[reconcileRequest]())
	defer queue.ShutDown()
	var logs bytes.Buffer
	informer := &Informer{
		queue:            queue,
		logger:           slog.New(slog.NewTextHandler(&logs, nil)),
		failedReconciles: make(map[string]struct{}),
	}
	var reconcileHealthy []bool
	informer.onReconcileHealth = func(healthy bool) {
		reconcileHealthy = append(reconcileHealthy, healthy)
	}
	req := reconcileRequest{key: "prod/web", resync: true}
	queue.Add(req)
	wantErr := errors.New("simulated reconciliation failure")

	if keepGoing := informer.processNextItem(context.Background(), func(context.Context, string, bool) error {
		return wantErr
	}); !keepGoing {
		t.Fatal("processNextItem() = false, want queue processing to continue")
	}
	if queue.NumRequeues(req) != 1 {
		t.Fatalf("NumRequeues = %d, want 1", queue.NumRequeues(req))
	}
	if len(reconcileHealthy) != 1 || reconcileHealthy[0] {
		t.Fatalf("reconcile health callbacks = %v, want [false]", reconcileHealthy)
	}
	got := logs.String()
	for _, want := range []string{"reconcile failed", "prod/web", wantErr.Error()} {
		if !strings.Contains(got, want) {
			t.Fatalf("log = %q, want substring %q", got, want)
		}
	}
}

func TestProcessNextItemClearsHealthOnlyForRecoveredKey(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue[reconcileRequest](workqueue.DefaultTypedControllerRateLimiter[reconcileRequest]())
	defer queue.ShutDown()
	recovered := reconcileRequest{key: "prod/recovered"}
	queue.Add(recovered)
	var gotHealthy []bool
	informer := &Informer{
		queue:            queue,
		logger:           slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		failedReconciles: map[string]struct{}{recovered.key: {}, "prod/still-broken": {}},
		onReconcileHealth: func(healthy bool) {
			gotHealthy = append(gotHealthy, healthy)
		},
	}

	if keepGoing := informer.processNextItem(context.Background(), func(context.Context, string, bool) error { return nil }); !keepGoing {
		t.Fatal("processNextItem() = false")
	}
	if len(gotHealthy) != 1 || gotHealthy[0] {
		t.Fatalf("health callbacks = %v, want [false] while another key still fails", gotHealthy)
	}
	if _, present := informer.failedReconciles[recovered.key]; present {
		t.Fatal("recovered key remains in failedReconciles")
	}
}
