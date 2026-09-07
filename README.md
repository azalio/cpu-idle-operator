# cpu-idle-operator

`cpu-idle-operator` is a Kubernetes node agent that exposes `cpu.idle` and
`cpu.max.burst` through pod annotations. Its default-enabled node-pressure
guard additionally controls `cgroup.freeze` for eligible idle-tier pods. It
runs as a DaemonSet and requires no CRDs, admission webhook, or control-plane
component.

## Maximum, Yield, and Progress

The controls act on different scheduler properties:

- **Maximum** is the CPU ceiling. Kubernetes derives `cpu.max` from
  `limits.cpu`; burst lets a pod spend unused allowance from earlier
  periods without changing that ceiling.
- **Yield** is whether a pod competes normally or steps aside for other
  runnable work. The idle tier changes this property with `cpu.idle`.
- **Progress** is the CPU time the workload actually receives.
- **Pause** is whether the workload can run at all. The node-pressure guard
  temporarily sets `cgroup.freeze=1` when foreground utilization stays high.

The idle tier gives CPU away; the burst tier spends only the pod's own
banked quota. Neither annotation grants a pod CPU taken from a neighbour,
and both annotations may be used together.

## Requirements

- Linux with unified cgroup v2 and kernel 5.15 or newer.
- Kubernetes client libraries and the Pod-level-resource/QoS calculations
  track Kubernetes 1.36.3. The repository's kind runtime test currently uses
  Kubernetes 1.33.1 for the apply/resync/revert path. Kubernetes 1.36 clusters
  are expected to keep the default-enabled `PodLevelResources` behavior;
  changing that feature gate can change the kubelet's QoS path calculation.
- The `systemd` cgroup driver is supported; `cgroupfs` support is experimental.
- The default Helm values assume a stock kubelet cgroup layout. Override
  `cgroupRoot` and `kubepodsName` for a non-default kubelet
  `--cgroup-root`.
- The agent runs as root and mounts `/sys/fs/cgroup` read-write so it can
  update pod cgroups.

Unsupported nodes stay alive but not ready, expose the gate failure reason,
and perform no cgroup writes. cgroup v1 and hybrid mode are unsupported.

For a kubelet with a non-default `--cgroup-root`, override both `cgroupRoot`
and `kubepodsName`. For example, kind commonly needs
`cgroupRoot=/sys/fs/cgroup/kubelet.slice` and
`kubepodsName=kubelet-kubepods`; the chart defaults remain correct for a
stock production kubelet.

## Install with Helm

From a repository checkout:

```sh
helm upgrade --install cpu-idle-operator ./deploy/helm/cpu-idle-operator
kubectl -n cpu-idle-system rollout status daemonset/cpu-idle-agent
```

The chart creates the `cpu-idle-system` namespace. See
[`values.yaml`](deploy/helm/cpu-idle-operator/values.yaml) for image,
resource, scheduling, cgroup path, metrics, health endpoint, and node-pressure
guard settings. The guard and its required Pod-patch RBAC are included and
enabled by the default chart install; no separate `guard-values.yaml` is
needed.

Do not remove the chart while annotated pods still have active cgroup state:
process shutdown intentionally does not revert annotation-owned tiers. First
remove the annotations from workload templates and quiesce controllers that
could recreate annotated Pods. Then remove them from existing Pods, run the
one-shot cleanup once in every agent container, and only then uninstall:

```bash
set -euo pipefail

# Keep these equal to the installed chart's cgroupRoot and kubepodsName.
cgroup_root=/sys/fs/cgroup
kubepods_name=kubepods

kubectl -n cpu-idle-system rollout status daemonset/cpu-idle-agent

kubectl annotate pods --all --all-namespaces \
  cpu.azalio.net/tier- cpu.azalio.net/burst-

agent_pods_file="$(mktemp)"
trap 'rm -f "${agent_pods_file}"' EXIT

kubectl -n cpu-idle-system get pods \
  -l app.kubernetes.io/name=cpu-idle-operator,app.kubernetes.io/component=agent \
  -o name >"${agent_pods_file}"
[[ -s "${agent_pods_file}" ]]

while IFS= read -r pod; do
  kubectl -n cpu-idle-system exec "$pod" -- /cpu-idle-agent \
    --revert-all \
    --cgroup-root="${cgroup_root}" \
    --kubepods-name="${kubepods_name}"
done <"${agent_pods_file}"

helm uninstall cpu-idle-operator
```

Each cleanup command prints the pods and state it reverted and exits non-zero
if any pod could not be cleaned. Do not continue to `helm uninstall` after a
failed cleanup.

## Pod annotations

| Annotation | Value | Effect |
|---|---|---|
| `cpu.azalio.net/tier` | `idle` | Sets `cpu.idle=1` on the pod cgroup. The pod yields CPU whenever non-idle work is runnable. Other non-empty values are ignored and produce a Kubernetes Event. |
| `cpu.azalio.net/burst` | any value | Sets `cpu.max.burst` to the pod's own `cpu.max` quota. Only key presence matters. A positive pod-level CPU limit qualifies; otherwise every regular and init container must declare a positive `limits.cpu`. If the live pod cgroup has no quota, the annotation remains inactive and produces a Kubernetes Event. |

The annotations are independent and may be used together.

### Idle tier

```yaml
metadata:
  annotations:
    cpu.azalio.net/tier: idle
```

### CPU quota burst

```yaml
metadata:
  annotations:
    cpu.azalio.net/burst: "true"
spec:
  containers:
    - name: workload
      resources:
        limits:
          cpu: "1"
```

Removing an annotation restores the corresponding cgroup setting.

Complete manifests are available in
[`config/samples/`](config/samples/). Annotation decisions and rejected
cgroup writes are reported as Kubernetes Events on the affected pod.

## Node-pressure guard

The guard protects foreground work when there is too little spare node CPU
left to harvest safely. It is enabled by default with the following values:

```yaml
guard:
  high: 0.70
  low: 0.60
  period: 5s
```

`high` and `low` are fractions of the node's aggregate CPU capacity used by
non-idle work. For example, `high: 0.70` means 70% across all logical CPUs,
not 70% of one CPU. Every `period`, the agent samples node CPU usage and
subtracts the CPU used by active idle-tier pods:

1. Two consecutive samples above `high` make the node hot. The guard writes
   `cgroup.freeze=1` to each eligible idle-tier pod cgroup.
2. The node stays hot throughout the hysteresis band between `low` and
   `high`.
3. Two consecutive samples below `low` make the node cool. The guard writes
   `cgroup.freeze=0` only to pod cgroups it previously froze.

With the default 5-second period, a hot/cool transition needs roughly 10
seconds of sustained load on the corresponding side of the threshold. A new
eligible pod discovered while the node is already hot is frozen on the next
sample.

An eligible pod must be Running, not terminating, carry
`cpu.azalio.net/tier=idle`, and already have live `cpu.idle=1`. CPU requests,
CPU limits, and the current `cpu.max` value do not affect freeze eligibility;
the guard never reads or writes `cpu.max` as part of normal operation.

`cgroup.freeze=1` pauses every process in the pod; this is a stop, not a CPU
limit or a small CPU allowance. Application traffic and health probes cannot
be served until the guard thaws the pod. Liveness probes may therefore restart
guarded workloads during a sustained hot interval; configure probes and use
this feature only for workloads that may be paused.

To tune the thresholds without an extra values file:

```sh
helm upgrade --install cpu-idle-operator ./deploy/helm/cpu-idle-operator \
  --set guard.high=0.80 \
  --set guard.low=0.65 \
  --set guard.period=10s
```

Keep `0 < low < high <= 1`. Set `guard.high=0` to disable new guard activity.
Before disabling it, run the `--revert-all` pass from the uninstall procedure
so no currently frozen pod is left behind.

For safe recovery, the guard:

- persists an internal ownership marker before freezing and records the exact
  `cgroup.freeze` transition;
- restores only while the live value still equals its own suppression value,
  preserving a different value written later by another actor;
- recovers owned state at the next startup while the guard remains enabled,
  and in explicit `--revert-all` mode;
- recognizes old `cpu.max` ownership markers only to undo suppression left by
  versions that implemented the guard as throttling. New markers and normal
  guard writes use only `cgroup.freeze`.

Normal process shutdown performs no cgroup writes. This keeps rolling updates
from changing workload policy; a surviving marker is recovered by the next
enabled agent. The chart's ClusterRole includes Pod `patch` for the ownership
marker. Kubernetes RBAC cannot restrict that verb to one annotation, so this
permission is part of the documented security boundary.

The ownership annotation is operator-internal state. Workloads must not set or
modify it themselves.

## VPA and in-place resize

Exclude pods carrying either tier from in-place vertical resizing. The kernel
rejects `cpu.weight` writes while `cpu.idle=1`, and it rejects lowering
`cpu.max` below an active `cpu.max.burst`. The agent reports those conflicts;
it does not race kubelet by temporarily removing a tier.

When the idle tier is removed successfully, the agent writes `cpu.idle=0`
first and then restores `cpu.weight` from the Pod's current effective CPU
request using kubelet's conversion. Pod-level requests, restartable init
containers, ordinary init containers, and Pod overhead are included.

## Observability

- `/healthz` reports process liveness.
- `/readyz` stays unavailable until the environment gate and informer cache
  are ready, and becomes unavailable after retryable reconcile or guard
  failures. Kernel policy conflicts intentionally handled as `EINVAL` are
  reported through Events and metrics and wait for a later resync rather than
  failing readiness.
- `/metrics` exports actual live tier membership, apply outcomes, resync
  drift, and environment-gate failures using bounded labels only.
- Kubernetes Events report applied, reverted, inactive, rejected, suppressed,
  and restored outcomes on the affected Pod.

## Security boundary

The container runs as uid 0 with all Linux capabilities dropped and without
`privileged: true`, but the writable `/sys/fs/cgroup` host mount still gives it
the practical ability to modify any Pod cgroup on its node. The meaningful
boundary is therefore enforced in code:

- normal cgroup writes are restricted to an exact pod-cgroup path and
  `cpu.idle`, `cpu.weight`, `cpu.max.burst`, or `cgroup.freeze`; `cpu.max` is
  allowlisted only to remove a legacy version-1 guard throttle during recovery,
  never for current guard control;
- no QoS slice, root cgroup, container scope, CRI socket, container runtime, or
  `/proc/<pid>/cgroup` path is used;
- the Pod informer is server-side scoped to the local node;
- Kubernetes API writes are limited to Events and internal ownership markers.

## Build and verification

```sh
make build
go test ./...
go vet ./...
(cd example/benchwork && go test ./... && go vet ./...)
make manifests
make check-manifests-drift
hack/check-manifests.sh
hack/check-readme-keys.sh
```

The Helm chart is the manifest source of truth. `make manifests` regenerates
[`config/base/`](config/base/), and CI rejects drift. The Go test suite covers
planning, QoS and resource calculations, cgroup path/write guards, lifecycle,
recovery, metrics, and Events. The kind suite and
[`hack/stand-probe.sh`](hack/stand-probe.sh) provide Linux runtime checks that
temporary-file unit tests cannot reproduce.

## Non-goals

- cgroup v1 or hybrid-mode support;
- CRDs, admission webhooks, leader election, or policy selectors;
- parsing a numeric burst amount from the annotation value;
- actively remediating VPA or in-place-resize conflicts;
- performance, capacity, or cost claims not backed by this repository's
  measurements.

## License

[Apache License 2.0](LICENSE)
