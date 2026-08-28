# internal/cgroup depends on github.com/opencontainers/cgroups, which only
# compiles for GOOS=linux (it needs unix.Openat2 and friends). This agent
# only ever runs inside a Linux DaemonSet, so `build` cross-compiles to
# linux/amd64 unconditionally — that also makes it work from a macOS or
# other non-Linux dev machine instead of failing there.
IMG ?= ghcr.io/azalio/cpu-idle-operator:latest
GOOS ?= linux
GOARCH ?= amd64
BIN := bin/cpi-idle-agent

# The Helm chart under HELM_CHART is the source of truth for config/base
# (see the "manifests" target below): config/base is generated output, not
# hand-maintained in parallel. HELM_RELEASE/HELM_NAMESPACE are fixed so the
# render is reproducible regardless of who runs it or what's installed in a
# live cluster -- `helm template` never talks to a cluster, these are just
# name/namespace substitutions.
HELM_CHART := deploy/helm/cpi-idle-operator
HELM_RELEASE := cpi-idle-operator
HELM_NAMESPACE := cpi-idle-system
CONFIG_BASE := config/base

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/agent

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: deploy
deploy:
	kustomize build config/base | kubectl apply -f -

# manifests renders config/base from the Helm chart. Run this after editing
# anything under deploy/helm/cpi-idle-operator and commit the result --
# config/base itself must never be hand-edited, see check-manifests-drift.
#
# The render is normalized before it is written: different Helm versions place
# blank lines around the "---" document separator differently, and without
# normalization check-manifests-drift fails purely because the CI runner's Helm
# is not byte-for-byte the developer's. Normalization = drop the "# Source:"
# comments, drop a leading separator, collapse runs of blank lines into one,
# and strip trailing blank lines. Whitespace-only differences carry no meaning
# in these manifests, so making the gate blind to them is correct, not a
# loosening: it still fails on any change to an actual key or value.
define render_template
	helm template $(HELM_RELEASE) $(HELM_CHART) --namespace $(HELM_NAMESPACE) --show-only templates/$(1) \
		| grep -v '^# Source:' \
		| awk 'NR==1 && /^---$$/{next} {print}' \
		| cat -s \
		| awk '{lines[NR]=$$0} END{last=NR; while (last>0 && lines[last] ~ /^[[:space:]]*$$/) last--; for(i=1;i<=last;i++) print lines[i]}' \
		> $(CONFIG_BASE)/$(1)
endef

.PHONY: manifests
manifests:
	$(call render_template,namespace.yaml)
	$(call render_template,rbac.yaml)
	$(call render_template,daemonset.yaml)

# check-manifests-drift fails if config/base no longer matches a fresh
# render of the Helm chart -- the two must never be allowed to diverge
# silently. Requires a git checkout (compares against the working tree).
.PHONY: check-manifests-drift
check-manifests-drift: manifests
	@if ! git diff --exit-code -- $(CONFIG_BASE); then \
		echo "ERROR: config/base is out of date with deploy/helm/cpi-idle-operator. Run 'make manifests' and commit the result." >&2; \
		exit 1; \
	fi
