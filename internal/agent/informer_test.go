package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

		informer := NewInformer(client, "node-a", time.Minute)

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
