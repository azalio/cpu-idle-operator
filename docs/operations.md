# Operations reference

This page contains the operational detail intentionally kept out of the
quick-start README.

## CPU controls

The operator manages three independent cgroup v2 mechanisms:

- `cpu.idle` changes scheduling priority. An idle cgroup runs only when
  non-idle work has no runnable CPU demand.
- `cpu.max.burst` lets a quota-limited Pod use accumulated CFS bandwidth. It
  does not raise or replace the Pod's `cpu.max` limit.
- `cgroup.freeze` stops every process in an eligible idle-tier Pod while the
  node-pressure guard is active.

## Requirements

- Linux with unified cgroup v2 and kernel 5.15 or newer.
- The `systemd` cgroup driver is supported; `cgroupfs` support is experimental.
- Kubernetes client libraries and Pod-level resource calculations track
  Kubernetes 1.36.3. The runtime E2E suite currently exercises Kubernetes
  1.33.1 as well.
- The default chart values assume a stock kubelet cgroup layout. For a
  non-default kubelet `--cgroup-root`, override both `cgroupRoot` and
  `kubepodsName`.
- The agent runs as root and mounts `/sys/fs/cgroup` read-write.

Unsupported nodes stay alive but not ready, expose the environment-gate
failure, and perform no cgroup writes. cgroup v1 and hybrid mode are not
supported.

For kind, the required overrides are commonly:

```yaml
cgroupRoot: /sys/fs/cgroup/kubelet.slice
kubepodsName: kubelet-kubepods
```

## Configuration

The Helm chart's [`values.yaml`](../deploy/helm/cpu-idle-operator/values.yaml)
is the complete configuration reference and the source of truth for
`config/base`.

The node-pressure guard defaults are:

```yaml
guard:
  high: 0.70
  low: 0.60
  period: 5s
```

`high` and `low` are fractions of total node CPU used by non-idle work. Two
consecutive samples above `high` make the node hot; two below `low` make it
cool. The node remains hot inside the hysteresis band.

An eligible Pod must be Running, not terminating, carry
`cpu.azalio.net/tier=idle`, and already have live `cpu.idle=1`. CPU requests,
CPU limits, and `cpu.max` do not affect guard eligibility.

Tune the guard with Helm values:

```sh
helm upgrade --install cpu-idle-operator ./deploy/helm/cpu-idle-operator \
  --set guard.high=0.80 \
  --set guard.low=0.65 \
  --set guard.period=10s
```

Keep `0 < low < high <= 1`. Setting `guard.high=0` disables new guard
activity. Run the cleanup procedure below before disabling it so no currently
frozen Pod is left behind.

## Guard recovery

Before freezing a Pod, the agent persists an internal ownership marker with
the exact `cgroup.freeze` transition. It then:

- restores only while the live value still matches its own suppression value;
- recovers owned state after an enabled-agent restart and in explicit
  `--revert-all` mode;
- recognizes legacy version-1 `cpu.max` markers only to remove throttles left
  by older releases.

Normal guard operation uses only `cgroup.freeze`. Normal process shutdown does
not modify workload cgroups, allowing a replacement agent to recover owned
state during a rolling update.

## Safe uninstall

Do not uninstall while annotated Pods still have active cgroup state. First
remove the annotations from workload templates and stop controllers from
recreating annotated Pods. Then remove them from existing Pods, run
`--revert-all` once in every agent container, and uninstall only after every
cleanup succeeds:

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

Each cleanup command reports what it reverted and exits non-zero if any Pod
could not be cleaned. Do not continue to `helm uninstall` after a failure.

## VPA and in-place resize

Exclude Pods carrying either tier from in-place vertical resizing. The kernel
rejects `cpu.weight` writes while `cpu.idle=1`, and rejects lowering `cpu.max`
below an active `cpu.max.burst`. The agent reports these conflicts instead of
racing kubelet.

When the idle tier is removed, the agent writes `cpu.idle=0` first and restores
`cpu.weight` from the Pod's current effective CPU request.

## Observability

- `/healthz` reports process liveness.
- `/readyz` becomes ready after the environment gate and informer cache are
  ready, and reports retryable reconcile or guard failures.
- `/metrics` exports tier membership, apply outcomes, resync drift, and
  environment-gate failures using bounded labels.
- Kubernetes Events report applied, reverted, inactive, rejected, frozen, and
  restored outcomes on the affected Pod.

## Security boundary

The container runs as uid 0 with all Linux capabilities dropped and without
`privileged: true`. The writable cgroup host mount still gives it the practical
ability to modify any Pod cgroup on its node.

The implementation restricts normal writes to an exact Pod cgroup and these
knobs: `cpu.idle`, `cpu.weight`, `cpu.max.burst`, and `cgroup.freeze`.
`cpu.max` is allowlisted only to remove a legacy guard throttle during
recovery. The agent does not use host PID/network namespaces, a CRI socket, or
container-runtime access.

The informer is server-side scoped to the local node. Kubernetes API writes
are limited to Events and the internal guard ownership marker. The chart's
ClusterRole includes Pod `patch`; RBAC cannot restrict that verb to one
annotation.

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

`make manifests` regenerates [`config/base`](../config/base) from the Helm
chart. CI rejects drift. The kind E2E suite additionally verifies readiness,
tier apply/revert, resync repair, and real guard freeze/thaw on cgroup v2.

## Non-goals

- cgroup v1 or hybrid-mode support;
- CRDs, admission webhooks, leader election, or policy selectors;
- parsing a numeric burst amount from the annotation value;
- actively remediating VPA or in-place-resize conflicts;
- performance, capacity, or cost claims not backed by repository measurements.
