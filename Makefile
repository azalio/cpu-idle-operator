# internal/cgroup depends on github.com/opencontainers/cgroups, which only
# compiles for GOOS=linux (it needs unix.Openat2 and friends). This agent
# only ever runs inside a Linux DaemonSet, so `build` cross-compiles to
# linux/amd64 unconditionally — that also makes it work from a macOS or
# other non-Linux dev machine instead of failing there.
IMG ?= ghcr.io/azalio/cpi-idle-operator:latest
GOOS ?= linux
GOARCH ?= amd64
BIN := bin/cpi-idle-agent

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/agent

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: deploy
deploy:
	kustomize build config/base | kubectl apply -f -
