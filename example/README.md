# Example: recovering an HTTP SLO with the idle tier

This directory is a self-contained, three-act demonstration of what the
`idle` tier actually does, on a live cluster, with a falsifiable pass/fail
gate at every step:

1. **Baseline.** A latency-sensitive HTTP service (`benchwork`) holds a
   fixed request rate inside an SLO of **p99 < 50 ms**.
2. **Break.** A CPU-saturating `stress-ng` pod — CPU requests but **no CPU
   limit**, sharing the node and the Burstable cgroup parent with the
   service — pushes the service out of its SLO.
3. **Recover.** One `kubectl annotate` puts the stressor on the idle tier.
   The service returns inside the SLO while `stress-ng` keeps making
   progress on whatever CPU is left over.

No pod is restarted, no limit is added, no requests are changed at any
point. The only thing that changes between act 2 and act 3 is the
stressor's pod-cgroup `cpu.idle`, flipped by the agent in response to the
annotation.

## What's in here

| File | Role |
|---|---|
| `benchwork/` | The load target: a Go HTTP server whose `/work` endpoint burns a fixed, deterministic amount of CPU per request (`BENCH_ITERATIONS`, default 1,000,000 xorshift rounds). Published as `ghcr.io/azalio/cpu-idle-operator-benchwork` by the same workflow that publishes the agent image. |
| `manifests/namespace.yaml` | The `cpu-idle-example` namespace. |
| `manifests/benchwork.yaml` | Deployment + Service for the foreground. CPU requests `500m`, **no CPU limit**. |
| `manifests/stressor.yaml` | The noisy neighbor: `stress-ng --cpu 0` in 60-second rounds with `--metrics-brief`, so its log prints a bogo-ops line every minute — that is the progress evidence. Same CPU requests `500m`, **no CPU limit**, and a required pod-affinity that co-schedules it onto benchwork's node. |
| `manifests/k6-job.yaml` | The load generator and the SLO gate in one object: a k6 Job driving a constant arrival rate at `/work`, with the SLO encoded as k6 thresholds (`p99 < 50 ms`, error rate ≤ 0.1%, zero dropped iterations). Thresholds failing make k6 exit non-zero, so **the Job's terminal state is the verdict**: `Complete` = SLO met, `Failed` = SLO violated. |

## Prerequisites

- The agent DaemonSet from `../config/base` deployed and Ready (see the
  repository README for what the environment gate requires: cgroup v2,
  kernel 5.15+).
- **At least two schedulable worker nodes.** The k6 Job carries a required
  anti-affinity against benchwork: measuring latency from the node you are
  deliberately saturating would corrupt the measurement. With a single
  node the k6 pod stays `Pending` — that is intentional.
- Nodes able to pull `ghcr.io` and `docker.io` images. If yours cannot
  (some managed clouds block or throttle foreign registries), mirror the
  three images into a registry your nodes can reach and rewrite the image
  references — `skopeo copy --override-os linux --override-arch amd64
  docker://<src> docker://<your-registry>/<name>` does the mirroring
  without a Docker daemon. (The run recorded below was executed on exactly
  such a cluster — nodes reaching neither ghcr.io nor docker.io — with all
  three images served from a private mirror.)

## Act 1: baseline

```bash
kubectl apply -f manifests/namespace.yaml -f manifests/benchwork.yaml
kubectl -n cpu-idle-example wait --for=condition=available deploy/benchwork

kubectl apply -f manifests/k6-job.yaml
kubectl -n cpu-idle-example wait --for=condition=complete --timeout=120s job/k6
kubectl -n cpu-idle-example logs job/k6 | grep 'name:work'
```

The `wait` succeeding is the gate. If the baseline does not pass on your
hardware, tune before proceeding — a baseline that can't hold the SLO
with an idle node proves nothing about the tiers. Lower the `RPS` env in
`manifests/k6-job.yaml`, or reduce `BENCH_ITERATIONS` in
`manifests/benchwork.yaml` (each request costs single-digit milliseconds
of CPU per million iterations on 2020s-era x86 cores).

## Act 2: break the SLO

```bash
kubectl apply -f manifests/stressor.yaml
kubectl -n cpu-idle-example wait --for=condition=ready pod/stressor

kubectl -n cpu-idle-example delete job k6
kubectl apply -f manifests/k6-job.yaml
kubectl -n cpu-idle-example wait --for=condition=failed --timeout=120s job/k6
kubectl -n cpu-idle-example logs job/k6 | grep 'name:work'
```

This gate is inverted: the run is only meaningful if the stressor
actually breaks the SLO (`wait --for=condition=failed` succeeds). If the
SLO survives on your node shape, raise `RPS` until the baseline still
passes but colocation fails, and redo act 1.

## Act 3: recover with the idle tier

```bash
kubectl -n cpu-idle-example annotate pod stressor cpu.azalio.net/tier=idle

kubectl -n cpu-idle-example get events \
  --field-selector involvedObject.name=stressor,reason=TierApplied
```

The `TierApplied` Event (`cpu.idle: result=applied reason=ok`) is the
agent confirming the write. Then re-run the same load:

```bash
kubectl -n cpu-idle-example delete job k6
kubectl apply -f manifests/k6-job.yaml
kubectl -n cpu-idle-example wait --for=condition=complete --timeout=120s job/k6
kubectl -n cpu-idle-example logs job/k6 | grep 'name:work'
kubectl -n cpu-idle-example logs stressor | grep 'bogo ops/s' | tail -2
```

Both halves matter: the Job completes (SLO met), and the stressor's most
recent 60-second round still reports a positive bogo-ops rate — the idle
tier cedes contested CPU, it does not freeze the pod.

To undo, remove the annotation and the agent restores the kubelet-owned
values, confirmed by `TierReverted` Events for `cpu.idle` and
`cpu.weight`:

```bash
kubectl -n cpu-idle-example annotate pod stressor cpu.azalio.net/tier-
```

Note that a stressor with neither annotation nor limit saturates its
node; delete it (and the namespace) when you're done.

## Demo mode: one long run with a live dashboard

The three gated runs prove the claim; for showing it to an audience there
is `manifests/k6-demo.yaml` — a single long k6 run (30 minutes, same 200
RPS) you act the scenario against while a dashboard draws the p99 line
live: flat, then a spike when the stressor lands, then the drop the
moment the annotation is applied.

Two dashboards are available:

- **k6's built-in web dashboard** (zero extra infrastructure): the demo
  Job already sets `K6_WEB_DASHBOARD=true`; reach it with
  `kubectl -n cpu-idle-example port-forward svc/k6-dashboard 5665:5665`
  and open http://localhost:5665.
- **Prometheus + Grafana** (`../monitoring/`): the demo Job also writes
  every metric over Prometheus remote write, with trends sent as native
  histograms — that detail matters, because the plain trend stats k6
  exports are cumulative over the whole run, and a cumulative p99 decays
  slowly instead of dropping when the SLO recovers. The provisioned
  Grafana dashboard ("cpu.idle demo: SLO recovery") charts the
  30-second-windowed p99/p95/median against the 50 ms SLO line, the
  offered load and errors, and per-pod CPU of benchwork and the stressor
  from cadvisor — the stressor's CPU staying busy after the annotation is
  the "still doing work" half of the story on the same screen.

```bash
kubectl apply -f monitoring/namespace.yaml
kubectl apply -f monitoring/prometheus.yaml -f monitoring/grafana.yaml
kubectl apply -f manifests/k6-demo.yaml
```

Prometheus and Grafana pin themselves to a node labeled
`role=monitoring` and tolerate a `dedicated=monitoring:NoSchedule` taint
— give them a dedicated node so the demo's contention never touches the
thing measuring it. To reach Grafana from outside, install an
ingress-nginx controller and apply `monitoring/ingress.yaml` (see the
comment in that file); Grafana then answers on the controller's
LoadBalancer IP, anonymous read-only, `admin`/`admin` to edit.

A rehearsal of this exact setup (same cluster as the measured run below):
windowed p99 held 11-13 ms under baseline, climbed past 140 ms within a
minute of the stressor starting, and was back under 20 ms within ~30
seconds of the annotation — while the stressor kept consuming all
leftover CPU. Stop the demo with
`kubectl -n cpu-idle-example delete job k6-demo` (its thresholds are
disabled via `DEMO=1`, so this Job's exit state means nothing).

## A measured run

Recorded 2026-08-28 on Yandex Managed Service for Kubernetes 1.35.1: two
2-vCPU (Intel Ice Lake) workers, Ubuntu 22.04.5, kernel 5.15.0-181,
cgroup v2, systemd cgroup driver, containerd 2.2.1. benchwork and
stressor on one node, k6 on the other, 200 RPS for 60 s per act,
defaults otherwise.

| Act | k6 Job state | HTTP p99 | HTTP errors | stress-ng bogo ops/s |
|---|---|---:|---:|---:|
| baseline | `Complete` | 9.36 ms | 0% | — |
| + stress-ng | `Failed` | 94.75 ms | 0% | 1870.9 |
| + `cpu.idle` via annotation | `Complete` | 16.31 ms | 0% | 1123.5 |

The pod-cgroup state on the node confirmed the mechanism at each step:
`cpu.idle` 0 → 1 after `TierApplied` (with `cpu.weight` reading `0`, how
the kernel renders an idle cgroup), back to `cpu.idle=0` /
`cpu.weight=20` after `TierReverted`.

Numbers are one run on one topology, published so you know what shape of
result to expect — not as a performance claim that transfers anywhere
else. On a 4-vCPU stand with 500 RPS the same three acts produced
baseline 8.45 ms → client-timeout breakage → 9.57 ms recovery with 96.6%
of stressor throughput retained; how much throughput the stressor keeps
depends entirely on how much CPU the foreground leaves idle.
