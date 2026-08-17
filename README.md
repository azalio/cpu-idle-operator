# cpi-idle-operator

A single-binary node agent for Kubernetes that gives pods opt-in access to
two cgroup v2 CPU tiers — `cpu.idle` and `cpu.max.burst` — through pod
annotations. No CRD, no admission webhook, no control-plane component: one
DaemonSet per node, watching only the pods scheduled to it.

Built as the reference implementation for a KubeCon talk about `cpu.idle`
and what cgroup v2's extended CPU knobs actually do (and don't do) for you.

## The two tiers: Maximum, Yield, Progress

Read this section before the rest of the document, or the two tiers below
will look like they contradict each other: one is for pods with no CPU
limit, the other only does anything for pods that *have* a limit.

Every pod's relationship to the CPU scheduler can be described along three
independent axes:

- **Maximum** — the upper bound on CPU the pod is allowed to consume. Set
  by `limits.cpu`, enforced in the kernel by `cpu.max`.
- **Yield** — whether the pod steps aside for other runnable, non-idle
  work, or competes for the CPU on equal footing. Controlled by
  `cpu.weight` and, at the extreme, `cpu.idle`.
- **Progress** — the CPU time the pod's own workload actually gets to
  make forward progress.

The two tiers this operator manages sit on different axes and never trade
one pod's Progress for another's:

- `cpu.azalio.net/tier: idle` sets `cpu.idle=1`. It gives away Yield: the
  pod steps aside whenever a non-idle neighbor is runnable, and only
  reclaims the CPU when nothing else wants it. It never asks a neighbor
  to give up Progress — it only ever cedes cycles, never takes them.
- `cpu.azalio.net/burst` sets `cpu.max.burst` equal to the pod's own CPU
  quota (`cpu.max`). It extends Maximum: the pod may spend, in a single
  burst, quota it did not use in earlier periods — its own banked
  allowance, not anyone else's. A pod with no `limits.cpu` has no quota to
  bank in the first place, so burst has nothing to act on (see
  [Annotations reference](#annotations-reference)).

Neither tier increases one pod's Progress at a neighbor's expense. `idle`
gives Progress away; `burst` only ever spends a pod's own previously-idle
quota. A pod can carry both annotations at once — they are independent and
apply on the same pod cgroup without conflict (measured; see
[Verifying the core facts yourself](#verifying-the-core-facts-yourself)).

## Annotations reference

| Annotation | Value | Effect |
|---|---|---|
| `cpu.azalio.net/tier` | `idle` | Sets `cpu.idle=1` on the pod cgroup. Any other non-empty value is treated as an unrecognized tier (reserved for future use): no-op plus a Kubernetes Event, not an error. |
| `cpu.azalio.net/burst` | any value, e.g. `"true"` | Sets `cpu.max.burst` to the pod's own `cpu.max` quota. The value itself is never parsed — presence of the key is what matters. Requires every container in the pod (including init containers) to declare a positive `limits.cpu`; otherwise the pod cgroup has no quota to burst against, and the agent emits an Event saying so instead of applying anything. |

Both keys live in exactly one place in the source tree —
[`internal/annotations/keys.go`](internal/annotations/keys.go) — and
[`hack/check-readme-keys.sh`](hack/check-readme-keys.sh) fails CI if this
table drifts from that file.

A pod may carry both annotations; see
[`config/samples/pod-both.yaml`](config/samples/pod-both.yaml) for a
worked example, and `pod-idle.yaml` / `pod-burst.yaml` for the single-tier
cases.

## Quick start

```
make build          # cross-compiles cmd/agent for linux/amd64
make docker-build    # builds the DaemonSet image
make deploy           # kustomize build config/base | kubectl apply -f -
```

`config/base` renders a namespace, a `ClusterRole` scoped to
`get/list/watch` on pods and `create/patch` on events, and the DaemonSet
itself. Apply one of the samples under `config/samples/` to see the agent
react to an annotation.

`config/base/daemonset.yaml` references `ghcr.io/azalio/cpi-idle-operator`.
[`.github/workflows/publish.yaml`](.github/workflows/publish.yaml) publishes
that image to GHCR on every merge to `main` (tags: `latest` and the commit
sha) and on every `v*` tag (the matching semver tag) -- **but not before
then**. Until this branch's first merge to `main`, `ghcr.io/azalio/cpi-idle-operator:latest`
does not exist yet, and `make deploy` against a real cluster will pull it
straight into `ImagePullBackOff`. To try the agent before that first
publish, build and load the image into your own cluster instead of relying
on the registry -- e.g. for `kind`: `make docker-build && kind load
docker-image ghcr.io/azalio/cpi-idle-operator:latest --name <your-cluster>`,
the same thing CI's own e2e job does (see
[`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)).

`config/base` itself is generated from the Helm chart under
[`deploy/helm/cpi-idle-operator`](deploy/helm/cpi-idle-operator), which is
the source of truth for the manifests. Run `make manifests` after changing
the chart and commit the regenerated `config/base` -- CI's
`check-manifests-drift` job fails the build if the two are out of sync.

## Supported environments

| Environment | Status |
|---|---|
| cgroup v2 unified, kernel 5.15+, `systemd` cgroup driver | **Supported.** Stand-verified (see below) and exercised by unit tests. |
| cgroup v2 unified, kernel 5.15+, `cgroupfs` cgroup driver | **Experimental.** Implemented, but not covered by e2e or a live stand run. The agent logs a warning once per environment check on this driver. |
| cgroup v1 | **Not supported.** `cpu.idle` does not exist for cgroup v1 entities. The agent fails its startup environment gate with a stated reason and performs zero cgroup writes. |
| cgroup v1/v2 hybrid | **Not supported**, same reason: the `cpu` controller stays on v1 in hybrid mode. |
| Kernel older than 5.15 | **Not supported.** `cpu.idle` for cgroup entities landed upstream in 5.15. |

On any unsupported node the agent stays alive, reports not-ready with the
specific reason, and never touches a cgroup file — it does not crash-loop
and it does not guess.

### Non-default kubelet `--cgroup-root`

`config/base/daemonset.yaml` ships `--cgroup-root=/sys/fs/cgroup` with the
top-level kubepods cgroup left at its default name, `kubepods` — correct
for a stock kubelet. Some clusters run kubelet with a non-default
`--cgroup-root` of their own (for example, `kind`'s kubeadm config sets it
to `/kubelet`), which nests the whole kubepods hierarchy one level deeper
and prefixes every kubepods slice/directory name with that root's own
basename (`kubelet-kubepods` on `kind`, measured directly on a kind node —
see `test/e2e/preflight_test.go`).

For a cluster shaped like that, pass both `--cgroup-root` (pointed at the
kubelet root's own cgroup directory) and `--kubepods-name` (the prefixed
kubepods name) so the agent's computed pod-cgroup path matches what that
kubelet actually creates. On `kind`, that pairing is
`--cgroup-root=/sys/fs/cgroup/kubelet.slice --kubepods-name=kubelet-kubepods`
— exactly what `test/e2e`'s own suite configures the DaemonSet with to
exercise the positive AC-10 scenario for real (see
[Testing: what's actually gated, and what isn't](#testing-whats-actually-gated-and-what-isnt)
below). Do not change `config/base/daemonset.yaml`'s own defaults for this:
they are correct for a real production kubelet, and a `kind`-shaped
override there would break every other cluster.

## VPA and in-place resize: excluded, not merely discouraged

**A pod carrying either tier is excluded from `VerticalPodAutoscaler`
in-place resize.** This isn't caution — it's a measured kernel behavior:

- Writing `cpu.weight` while `cpu.idle=1` returns `EINVAL` for any value.
- Lowering the CPU quota (`cpu.max`) below the pod's configured
  `cpu.max.burst` also returns `EINVAL`.

In-place resize writes exactly those two things. Once a pod is running
under either tier, a resize attempt from VPA — or from `kubectl` directly
— will be rejected by the kernel with `EINVAL`. The agent detects and
reports this conflict; it does not resolve it, retry around it, or revert
the tier to let the resize through.

## Security Boundaries

This DaemonSet mounts `/sys/fs/cgroup` read-write and runs as uid 0. That
is, de facto, the ability to change the cgroup of *any* pod on the node,
not just the one the agent is reasoning about — this has been measured
directly, not assumed: a container with every Linux capability dropped
and `privileged: false` still successfully wrote another pod's cgroup
file through this mount. Calling that "minimal privileges" would be
false, so this document doesn't.

What keeps the blast radius bounded is not the container's privilege
level — it has none worth mentioning — but everything the agent's own
code chooses never to do:

- It reads and writes exactly four cgroup files: `cpu.idle`, `cpu.weight`,
  `cpu.max`, `cpu.max.burst`. Nothing else under `/sys/fs/cgroup` is ever
  touched.
- Its Kubernetes RBAC is `get/list/watch` on pods and `create/patch` on
  events — nothing more — and its informer is scoped to its own node via
  a field selector.
- It never calls the CRI socket, never talks to containerd, and never
  reads `/proc/<pid>/cgroup`; the pod-cgroup path is derived only from the
  pod's UID and QoS class.
- The container itself never sets `privileged: true` and never requests
  `SYS_ADMIN`.
- Setting a tier only affects the pod that carries the annotation. There
  is no annotation, flag, or code path that lets one pod's owner take CPU
  away from a neighbor.

None of that changes the first paragraph. The compensating properties
bound what the agent *chooses* to do; they do not change what the mount
and the uid *allow* it to do.

## Non-goals

- No CRD, no control-plane controller, no admission webhook, and no gate
  on which pod owners may set either annotation.
- No support for cgroup v1 or hybrid mode.
- No active remediation of the VPA / in-place-resize conflict above — it
  is detected and reported, not fixed.
- No numeric override of the burst amount through an annotation value;
  the burst is always the pod's own quota.
- No performance claims about `cpu.max.burst`'s effect on tail latency —
  that effect has not been measured.
- No claims that adopting either tier changes how many nodes a cluster
  needs or what running it costs.
- No published latency figure for how long a tier takes to apply after a
  pod or annotation appears. That number has never been measured on this
  project; the agent applies tiers on an event-driven watch with a
  periodic resync as a safety net, and that's what's currently known.

## Verifying the core facts yourself

This repository doesn't ask you to take its claims on faith.
[`hack/stand-probe.sh`](hack/stand-probe.sh) reproduces the load-bearing
facts above with a single command, run directly on a cgroup v2 node with
`sudo` and (optionally) `kubectl`:

```
sudo hack/stand-probe.sh --dry-run   # see the plan without touching anything
sudo hack/stand-probe.sh              # create probe pods and run the checks
```

It prints a table of check / expected / observed and exits non-zero on
any mismatch. Among what it reproduces: `cpu.idle=1` surviving a
`kubelet` restart and a full reconciliation cycle, `cpu.weight` writes
being rejected with `EINVAL` while `cpu.idle=1`, the weight reverting to
the kernel default (not the request-derived value) when `cpu.idle` is
cleared, and both tiers coexisting on one pod cgroup without conflict.

## Testing: what's actually gated, and what isn't

Unit tests and `go vet` gate every merge. So does an e2e suite against a
real [`kind`](https://kind.sigs.k8s.io/) cluster with a real DaemonSet
from `config/base`, and it does prove the agent applies and reverts a tier
on `kind`, for real: `kind` sets kubelet's `cgroupRoot` to `/kubelet`, so
pods land under `kubelet.slice/kubelet-kubepods…`, not the plain
`kubepods.slice` this agent expects at its default `--cgroup-root`. That
mismatch was an open question; it's closed — not by reasoning about it, but
by measuring the exact divergent layout on a live node and making the
top-level kubepods cgroup name configurable (`--kubepods-name`, see
[Non-default kubelet `--cgroup-root`](#non-default-kubelet---cgroup-root)
above) so the agent can be pointed at it.

`test/e2e`'s suite deploys `config/base`'s unmodified manifests, then
patches only the deployed DaemonSet's `--cgroup-root`/`--kubepods-name`
arguments for this `kind`-only reason (`config/base/daemonset.yaml` itself
never carries either override — its defaults stay correct for a real
production kubelet). A dedicated pre-flight test
(`TestPreflightKindCgroupViewConsistency`) proves, before anything else
runs, that the agent's computed pod-cgroup path converges with the path
`kind`'s kubelet actually creates; the main scenario
(`TestKindApplyAndRevert`) then applies the idle tier through the live
DaemonSet, reads `cpu.idle=1` back from the node's real cgroupfs, removes
the annotation, and confirms both `cpu.idle=0` and the restored
request-derived `cpu.weight`. Both are required, merge-blocking checks. The
same positive path is also covered independently by `hack/stand-probe.sh`
against a real node with a stock kubelet configuration.

## License

[Apache License 2.0](LICENSE)
