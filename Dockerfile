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

ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 produces the static Linux binary required by the distroless
# runtime image. BuildKit supplies TARGETOS/TARGETARCH for cross-platform
# builds. Docker-compatible engines that omit those automatic args still
# build correctly for the selected builder image instead of silently forcing
# an amd64 binary into (for example) an arm64 image.
RUN target_os="${TARGETOS:-$(go env GOOS)}"; \
    target_arch="${TARGETARCH:-$(go env GOARCH)}"; \
    CGO_ENABLED=0 GOOS="${target_os}" GOARCH="${target_arch}" \
    go build -trimpath -ldflags="-s -w" -o /out/cpu-idle-agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:latest

COPY --from=builder /out/cpu-idle-agent /cpu-idle-agent

ENTRYPOINT ["/cpu-idle-agent"]
