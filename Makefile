# The agent only runs inside a Linux DaemonSet, so `build` targets
# linux/amd64 by default even when invoked on a macOS development host.
# GOOS/GOARCH remain overridable for other Linux image architectures.
IMG ?= ghcr.io/azalio/cpu-idle-operator:latest
GOOS ?= linux
GOARCH ?= amd64
BIN := bin/cpu-idle-agent

# The Helm chart is the source of truth for config/base (see the
# "manifests" target below): config/base is generated output, not
# hand-maintained in parallel.
CONFIG_BASE := config/base

.PHONY: build
build:
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/agent

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: deploy
deploy:
	kustomize build config/base | kubectl apply -f -

# manifests renders config/base from the Helm chart. Run this after editing
# anything under deploy/helm/cpu-idle-operator and commit the result --
# config/base itself must never be hand-edited, see check-manifests-drift.
#
.PHONY: manifests
manifests:
	hack/render-manifests.sh $(CONFIG_BASE)

# check-manifests-drift fails if config/base no longer matches a fresh
# render of the Helm chart. It renders into a temporary directory and never
# rewrites the files being checked, so it also works correctly in a dirty
# worktree containing an intentional chart/base change.
.PHONY: check-manifests-drift
check-manifests-drift:
	@tmp_dir="$$(mktemp -d)"; \
	trap 'rm -f "$${tmp_dir}/namespace.yaml" "$${tmp_dir}/rbac.yaml" "$${tmp_dir}/daemonset.yaml"; rmdir "$${tmp_dir}"' EXIT; \
	hack/render-manifests.sh "$${tmp_dir}"; \
	status=0; \
	for manifest in namespace.yaml rbac.yaml daemonset.yaml; do \
		diff -u "$(CONFIG_BASE)/$${manifest}" "$${tmp_dir}/$${manifest}" || status=1; \
	done; \
	if [ "$${status}" -ne 0 ]; then \
		echo "ERROR: config/base is out of date with deploy/helm/cpu-idle-operator. Run 'make manifests'." >&2; \
		exit 1; \
	fi
