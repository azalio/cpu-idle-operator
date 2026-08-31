# cpu-idle-agent is a single static binary that never shells out (it talks
# to the Kubernetes API and to cgroupfs directly), so the runtime image
# needs no shell and no package manager — distroless "static" is enough.
# The image keeps root as its default user on purpose: the DaemonSet's
# securityContext sets runAsUser: 0 because DAC root is what lets the
# agent write another pod's cgroup files through the hostPath mount (see
# config/base/daemonset.yaml's SECURITY NOTE).
FROM golang:1.26 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO_ENABLED=0 plus GOOS=linux is required regardless of build host:
# internal/cgroup only compiles under Linux (cgroup v2 syscalls), and a
# static binary is what makes the distroless "static" base work at all.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/cpu-idle-agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:latest

COPY --from=builder /out/cpu-idle-agent /cpu-idle-agent

ENTRYPOINT ["/cpu-idle-agent"]
