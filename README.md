# cpu-idle-operator

`cpu-idle-operator` is a Kubernetes node agent that exposes two cgroup v2
CPU controls through pod annotations: `cpu.idle` and `cpu.max.burst`.
It runs as a DaemonSet and requires no CRDs, admission webhook, or
control-plane component.

## Requirements

- Linux with unified cgroup v2 and kernel 5.15 or newer.
- The `systemd` cgroup driver is supported; `cgroupfs` support is experimental.
- The default Helm values assume a stock kubelet cgroup layout. Override
  `cgroupRoot` and `kubepodsName` for a non-default kubelet
  `--cgroup-root`.
- The agent runs as root and mounts `/sys/fs/cgroup` read-write so it can
  update pod cgroups.

## Install with Helm

From a repository checkout:

```sh
helm upgrade --install cpu-idle-operator ./deploy/helm/cpu-idle-operator
kubectl -n cpu-idle-system rollout status daemonset/cpu-idle-agent
```

The chart creates the `cpu-idle-system` namespace. See
[`values.yaml`](deploy/helm/cpu-idle-operator/values.yaml) for image,
resource, scheduling, cgroup path, metrics, and health endpoint settings.

To uninstall:

```sh
helm uninstall cpu-idle-operator
```

## Pod annotations

| Annotation | Value | Effect |
|---|---|---|
| `cpu.azalio.net/tier` | `idle` | Sets `cpu.idle=1` on the pod cgroup. The pod yields CPU whenever non-idle work is runnable. Other non-empty values are ignored and produce a Kubernetes Event. |
| `cpu.azalio.net/burst` | any value | Sets `cpu.max.burst` to the pod's own `cpu.max` quota. Only key presence matters. Every regular and init container must declare a positive `limits.cpu`; otherwise the annotation has no effect and produces a Kubernetes Event. |

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

## License

[Apache License 2.0](LICENSE)
