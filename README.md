# cpu-idle-operator

`cpu-idle-operator` is a Kubernetes node agent that manages three Linux
cgroup v2 CPU controls for annotated Pods: idle scheduling, CPU quota
bursting, and automatic freezing of idle workloads under node pressure.

It runs as a DaemonSet. There are no CRDs, admission webhooks, or
control-plane components.

## What it does

| Control | Enabled by | Effect |
|---|---|---|
| `cpu.idle` | `cpu.azalio.net/tier: idle` | Yields CPU to non-idle workloads. |
| `cpu.max.burst` | `cpu.azalio.net/burst: "true"` | Enables CFS quota burst; requires `limits.cpu`. |
| `cgroup.freeze` | Automatic pressure guard | Pauses active idle-tier Pods while node CPU is hot. |

The two annotations are independent and may be used together. Removing one
restores the corresponding cgroup state. CPU burst uses the Pod's existing
quota and never changes `cpu.max`.

> [!WARNING]
> Freezing pauses every process in the Pod, including traffic handling and
> health probes. With the default guard enabled, use the idle tier only for
> workloads that may be paused.

## Install

You need Linux nodes with unified cgroup v2, kernel 5.15 or newer, and a
supported `systemd` cgroup driver. The agent runs as root and mounts
`/sys/fs/cgroup` read-write.

```sh
git clone --depth 1 https://github.com/azalio/cpu-idle-operator.git
cd cpu-idle-operator

helm upgrade --install cpu-idle-operator ./deploy/helm/cpu-idle-operator
kubectl -n cpu-idle-system rollout status daemonset/cpu-idle-agent
```

The chart creates the namespace, RBAC, and one agent per selected node. The
pressure guard is enabled by default.

Add the annotations to a workload's Pod template:

```yaml
spec:
  template:
    metadata:
      annotations:
        cpu.azalio.net/tier: idle
        cpu.azalio.net/burst: "true"
    spec:
      containers:
        - name: app
          resources:
            limits:
              cpu: "1" # required only for CPU burst
```

All settings and their defaults are documented in
[`values.yaml`](deploy/helm/cpu-idle-operator/values.yaml). For a kubelet with
a non-default `--cgroup-root`, override both `cgroupRoot` and `kubepodsName`.

Before uninstalling, follow the
[`--revert-all` procedure](docs/operations.md#safe-uninstall) so annotated or
frozen Pods are not left with stale cgroup state.

## How it works

1. A DaemonSet agent watches Running Pods assigned to its own node.
2. The agent resolves each Pod's exact cgroup and reconciles `cpu.idle` and
   `cpu.max.burst` from its annotations. A periodic resync repairs drift.
3. Every 5s (`guard.period`), the guard measures aggregate CPU used by
   non-idle work. With the defaults, two samples above `guard.high: 0.70`
   freeze eligible idle-tier Pods; two below `guard.low: 0.60` thaw them.
4. Before freezing, the agent records ownership on the Pod. It restores only
   transitions it owns, so it does not overwrite a later change by another
   actor.

Kubernetes Events report applied, reverted, inactive, suppressed, and restored
states. `/metrics`, `/healthz`, and `/readyz` expose runtime status.

The guard controls only `cgroup.freeze`; normal guard operation never reads
or writes `cpu.max`.

For guard tuning, safe recovery, observability, security boundaries, and
development commands, see the [operations reference](docs/operations.md).

Licensed under the [Apache License 2.0](LICENSE).
