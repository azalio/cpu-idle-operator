// revertall.go implements the --revert-all one-shot mode: the explicit,
// human-triggered operation that clears every idle/burst tier this agent
// ever applied on a node before the operator itself is removed from the
// cluster (resolution T-007). It intentionally does not reuse Lifecycle:
// that type's whole job is the long-running informer/workqueue/HTTP-server
// loop this mode must never start (INV-4's counterpart for revert-all is
// "no watch, no listener, one List, then exit").
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/azalio/cpu-idle-operator/internal/apply"
	"github.com/azalio/cpu-idle-operator/internal/cgroup"
	"github.com/azalio/cpu-idle-operator/internal/config"
	"github.com/azalio/cpu-idle-operator/internal/envgate"
	"github.com/azalio/cpu-idle-operator/internal/observe"
	"github.com/azalio/cpu-idle-operator/internal/qos"
)

// RevertAllOptions carries RunRevertAll's dependencies. cmd/agent/main.go
// only ever sets Client and Logger in production; every other field
// defaults to the same construction Lifecycle's ready branch uses (see
// lifecycle.go's Run). Tests substitute GateCheck/Uname to pin a cgroup
// driver without a real cgroup v2 mount underneath them, and Applier to
// journal calls without touching a filesystem.
type RevertAllOptions struct {
	// Client is the Kubernetes API client used for the one-shot pod List.
	// Required.
	Client kubernetes.Interface
	// Logger receives structured log lines. Defaults to slog.Default() when
	// nil.
	Logger *slog.Logger
	// Uname is the environment gate's kernel-release source, passed through
	// to GateCheck. Defaults to the real uname(2) syscall when nil; tests
	// pin a release string instead. See GateCheckFunc's doc comment
	// (lifecycle.go) for why this indirection exists.
	Uname envgate.UnameFunc
	// GateCheck decides which cgroup driver this pass computes pod cgroup
	// paths with. Defaults to envgate.Check when nil.
	GateCheck GateCheckFunc
	// EventRecorder is the client-go recorder Kubernetes Events flow
	// through when Applier is nil. Defaults to a broadcaster wired to
	// Client's event sink; tests inject a record.NewFakeRecorder to assert
	// on emitted Events without a real API server.
	EventRecorder record.EventRecorder
	// Applier performs each pod's actual revert. Defaults to a real
	// *apply.Applier (via apply.NewApplier), built from the driver
	// GateCheck reports, when nil. Tests substitute a call-journaling fake
	// to prove a failing pod does not stop the pass over the rest (VC2).
	Applier Applier
	// Out receives the printed result table. Defaults to os.Stdout when
	// nil.
	Out io.Writer
}

// RunRevertAll runs the --revert-all one-shot mode for cfg.NodeName: it
// lists the node's pods with a single List call — never an informer, never
// a watch — and calls Applier.Revert (reusing ST-008's write order and
// EINVAL/ErrCgroupGone handling wholesale, never duplicating it) for every
// pod whose cgroup Snapshot shows an active tier right now. It prints a
// result table (pod / tiers cleared / result) to opts.Out and starts no
// HTTP server of any kind: this mode is meant to run as a standalone Job
// immediately before the operator is removed from the cluster, not as
// anything the long-running agent process ever reaches on its own.
//
// A pod's active tier is read from its live cgroup Snapshot, not its tier
// annotation: the annotation may already have been removed (or never
// existed) while the cgroup itself was never reverted, which is exactly
// the gap this mode exists to close.
//
// RunRevertAll returns nil when every pod either reverted successfully or
// had nothing to revert, including the empty-node case. It returns a
// non-nil error when at least one pod failed to revert — except
// cgroup.ErrCgroupGone, the expected race of a pod deleted between the
// List call and this pass reaching it, which never counts as a failure —
// and it never stops on the first failure: every pod in the list is
// attempted regardless of an earlier pod's outcome. It also returns a
// non-nil error when every pod reverted cleanly but the result table itself
// failed to reach opts.Out (see printRevertTable): that table is this
// mode's only human-facing report, so a run whose report never actually
// arrived must not exit 0.
func RunRevertAll(ctx context.Context, cfg config.Config, opts RevertAllOptions) error {
	if opts.Client == nil {
		return errors.New("agent: RunRevertAll: Client must not be nil")
	}
	if cfg.NodeName == "" {
		return errors.New("agent: RunRevertAll: Config.NodeName must not be empty")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}

	gateResult, err := revertAllGateCheck(opts)(cfg.CgroupRoot, cfg.KubepodsName, revertAllUname(opts))
	if err != nil {
		return fmt.Errorf("agent: revert-all: environment gate check: %w", err)
	}
	if !gateResult.Ready {
		return fmt.Errorf("agent: revert-all: environment gate not ready: %s", gateResult.Reason)
	}

	applier := opts.Applier
	if applier == nil {
		applier = defaultRevertAllApplier(cfg, gateResult.Driver, opts)
	}

	pods, err := listNodePods(ctx, opts.Client, cfg.NodeName)
	if err != nil {
		return fmt.Errorf("agent: revert-all: list pods: %w", err)
	}

	results := make([]revertResult, 0, len(pods))
	failures := 0
	for i := range pods {
		result := revertPod(ctx, applier, cfg.CgroupRoot, cfg.KubepodsName, gateResult.Driver, &pods[i])
		if result.err != nil {
			failures++
			logger.Error("agent: revert-all: pod revert failed", "pod", result.key, "error", result.err)
		}
		results = append(results, result)
	}

	tableErr := printRevertTable(out, results)
	if tableErr != nil {
		logger.Error("agent: revert-all: failed to print result table", "error", tableErr)
	}

	if failures > 0 {
		return fmt.Errorf("agent: revert-all: %d of %d pods failed to revert", failures, len(pods))
	}
	if tableErr != nil {
		return fmt.Errorf("agent: revert-all: %w", tableErr)
	}
	return nil
}

// revertAllGateCheck returns opts.GateCheck, defaulting to envgate.Check.
func revertAllGateCheck(opts RevertAllOptions) GateCheckFunc {
	if opts.GateCheck != nil {
		return opts.GateCheck
	}
	return envgate.Check
}

// revertAllUname returns opts.Uname, defaulting to the real uname(2)
// syscall.
func revertAllUname(opts RevertAllOptions) envgate.UnameFunc {
	if opts.Uname != nil {
		return opts.Uname
	}
	return realUname
}

// defaultRevertAllApplier builds this pass's production Applier: a fresh
// Prometheus registry (never exposed on an HTTP endpoint — this mode
// starts no server) plus an Event sink wired to opts.Client, matching
// Lifecycle's own ready-branch construction (lifecycle.go's Run) so a
// reverted pod is reported through the exact same Recorder/EventRecorder
// pairing CCR-1 requires everywhere else in this operator.
func defaultRevertAllApplier(cfg config.Config, driver cgroup.Driver, opts RevertAllOptions) Applier {
	eventRecorder := opts.EventRecorder
	if eventRecorder == nil {
		broadcaster := record.NewBroadcaster()
		broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: opts.Client.CoreV1().Events("")})
		eventRecorder = broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: componentName, Host: cfg.NodeName})
	}
	registry := prometheus.NewRegistry()
	recorder := observe.NewRecorder(registry, eventRecorder, cfg.NodeName)
	return apply.NewApplier(cfg.CgroupRoot, cfg.KubepodsName, driver, recorder, observe.NewEventRecorder(eventRecorder))
}

// listNodePods lists nodeName's pods with a single call, scoped
// server-side via the spec.nodeName field selector (the same scope
// Informer uses), and sorted by namespace/name so the printed table and
// this pass's own iteration order are deterministic.
func listNodePods(ctx context.Context, client kubernetes.Interface, nodeName string) ([]corev1.Pod, error) {
	list, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		if list.Items[i].Namespace != list.Items[j].Namespace {
			return list.Items[i].Namespace < list.Items[j].Namespace
		}
		return list.Items[i].Name < list.Items[j].Name
	})
	return list.Items, nil
}

// revertResult is one pod's outcome: a row in the printed table and an
// input to RunRevertAll's overall exit-code decision.
type revertResult struct {
	// key is the pod's namespace/name, for the printed table.
	key string
	// cleared names the tiers this pass found active on the pod's cgroup
	// ("idle", "burst", "idle+burst", or "none").
	cleared string
	// status is the human-readable outcome ("ok", "none", "gone", or
	// "error").
	status string
	// err is non-nil only for a real failure — never for cgroup.ErrCgroupGone,
	// which is folded into status "gone" instead.
	err error
}

// revertPod reverts a single pod's currently active tiers, reusing
// apply.Applier.Revert wholesale for the actual writes: INV-2/INV-7's
// write order lives there, once, and this function must never re-derive
// it. It reads the pod's live cgroup Snapshot itself rather than trusting
// the pod's tier annotation, so a pod whose annotation was already removed
// but whose cgroup was never reverted — the exact gap this mode exists to
// close — is still caught.
func revertPod(ctx context.Context, applier Applier, cgroupRoot, kubepodsName string, driver cgroup.Driver, pod *corev1.Pod) revertResult {
	result := revertResult{key: pod.Namespace + "/" + pod.Name}

	dir, err := cgroup.PodCgroupPath(cgroupRoot, kubepodsName, driver, qos.ToCgroupClass(qos.ClassOf(pod.Spec)), string(pod.UID))
	if err != nil {
		result.status = "error"
		result.err = fmt.Errorf("pod cgroup path: %w", err)
		return result
	}

	snapshot, err := apply.ReadSnapshot(dir)
	if err != nil {
		if errors.Is(err, cgroup.ErrCgroupGone) {
			// Intent: the pod raced to deletion between this pass's List
			// call and reaching it here — the expected race the subtask
			// names explicitly, not a failure.
			result.status = "gone"
			return result
		}
		result.status = "error"
		result.err = fmt.Errorf("read snapshot: %w", err)
		return result
	}

	result.cleared = describeActiveTiers(snapshot)
	if result.cleared == "none" {
		result.status = "none"
		return result
	}

	if err := applier.Revert(ctx, pod, snapshot); err != nil {
		if errors.Is(err, cgroup.ErrCgroupGone) {
			// Intent: Applier is an interface here (a test double may
			// choose to surface this race explicitly), even though the
			// production *apply.Applier already folds it into a nil
			// return itself (see internal/apply/revert.go).
			result.status = "gone"
			return result
		}
		result.status = "error"
		result.err = err
		return result
	}

	result.status = "ok"
	return result
}

// describeActiveTiers reports which of the two tiers snapshot shows active
// right now, for the printed table and revertPod's decision to call
// Applier.Revert at all — "none" when neither cpu.idle nor cpu.max.burst
// carries an active value.
func describeActiveTiers(snapshot apply.Snapshot) string {
	var tiers []string
	if snapshot.IdleActive {
		tiers = append(tiers, "idle")
	}
	if snapshot.Burst > 0 {
		tiers = append(tiers, "burst")
	}
	if len(tiers) == 0 {
		return "none"
	}
	return strings.Join(tiers, "+")
}

// printRevertTable prints results as a pod / cleared-tiers / result table
// and reports whether that table actually reached out. A tabwriter only
// buffers each Fprintln/Fprintf below into its internal column layout --
// the real write to out happens at Flush, so that is the one error this
// function surfaces; the per-line writes are discarded explicitly, since
// any real I/O failure among them would resurface at Flush anyway.
func printRevertTable(out io.Writer, results []revertResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "POD\tCLEARED\tRESULT")
	for _, result := range results {
		cleared := result.cleared
		if cleared == "" {
			cleared = "-"
		}
		status := result.status
		if result.err != nil {
			status = fmt.Sprintf("error: %v", result.err)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", result.key, cleared, status)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush result table: %w", err)
	}
	return nil
}
