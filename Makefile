VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/ewhauser/celld-operator
LDFLAGS := -s -w -X main.version=$(VERSION)
GO_PACKAGES := ./...
RACE ?= -race
GOLANGCI_LINT_VERSION ?= v2.13.2
GOLANGCI_LINT := GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
# A native lint binary that can analyze another GOOS; `go run` under GOOS=linux
# would build a Linux executable that cannot run on a macOS host.
GOLANGCI_LINT_BIN := bin/tools/golangci-lint
RUNTIME_IMAGE := ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8
HOST_GOARCH := $(shell go env GOARCH)

.PHONY: check check-full build test vet fmt lint lint-linux lint-new test-linux image clean

# Linux-only files (launcher process handling, build tags) are invisible to a
# macOS lint run; lint-linux analyzes the Linux build so they cannot reach CI red.
check: build test lint lint-linux

# Everything check does plus the suites that need Docker or envtest binaries.
check-full: check test-linux test-envtest

build:
	go build -o /dev/null $(GO_PACKAGES)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/celld-operator ./cmd/celld-operator
	CGO_ENABLED=0 go build -trimpath -o bin/celld-launcher ./cmd/celld-launcher

test:
	go test $(RACE) $(GO_PACKAGES)

# envtest runs the reconciler against a real kube-apiserver and etcd (no kubelet).
# The suite skips when KUBEBUILDER_ASSETS is unset, so plain `make test` stays hermetic.
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1

.PHONY: test-envtest
test-envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test $(RACE) ./internal/controller -run 'TestEnvtest' -count=1

vet:
	go vet $(GO_PACKAGES)

fmt:
	$(GOLANGCI_LINT) fmt ./...

lint:
	$(GOLANGCI_LINT) run ./...

$(GOLANGCI_LINT_BIN):
	GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOBIN=$(CURDIR)/bin/tools go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint-linux: $(GOLANGCI_LINT_BIN)
	GOOS=linux $(GOLANGCI_LINT_BIN) run ./...

# Cross-compile the platform-sensitive packages' tests and run them on Linux in
# the pinned runtime image (which supplies /bin/sh for the launcher fixtures).
# No race detector: that needs cgo. Requires Docker.
test-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_GOARCH) go test -c -o bin/tests/launcher.test ./internal/launcher
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_GOARCH) go test -c -o bin/tests/controller.test ./internal/controller
	docker run --rm --platform linux/$(HOST_GOARCH) -e TZ=UTC -v $(CURDIR)/bin/tests:/t:ro -v $(CURDIR):/src:ro -w /src/internal/launcher --entrypoint /t/launcher.test $(RUNTIME_IMAGE) -test.count=1
	docker run --rm --platform linux/$(HOST_GOARCH) -e TZ=UTC -v $(CURDIR)/bin/tests:/t:ro -v $(CURDIR):/src:ro -w /src/internal/controller --entrypoint /t/controller.test $(RUNTIME_IMAGE) -test.count=1

lint-new:
	$(GOLANGCI_LINT) run --new-from-rev=HEAD ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin

.PHONY: qualification-replay qualification-local
qualification-replay:
	sh hack/qualification/replay.sh

# Explicit opt-in. Each run uses isolated local Docker resources and a new output directory.
qualification-local:
	.qualification-venv/bin/python hack/qualification/run.py --scenario $(SCENARIO) --output $(OUTPUT)

.PHONY: qualification-test
qualification-test:
	.qualification-venv/bin/python -m unittest discover -s hack/qualification -p 'test_*.py'

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
.PHONY: generate manifests-check integration
generate:
	$(CONTROLLER_GEN) object crd paths=./api/... output:crd:artifacts:config=config/crd

manifests-check:
	python3 hack/check-generated.py

integration: build
	go run ./hack/integration

.PHONY: integration-bucket
integration-bucket:
	go run ./hack/integration --bucket-lifecycle

.PHONY: integration-persistent
integration-persistent:
	go run ./hack/integration --persistent-lifecycle

.PHONY: chart-sync chart-check chart-package
chart-sync:
	python3 hack/sync-chart.py

chart-check:
	python3 hack/sync-chart.py --check
	helm lint charts/celld-operator --strict
	.qualification-venv/bin/python hack/test-chart.py
	python3 -m unittest discover -s hack -p test_release.py

chart-package: chart-check
	helm package charts/celld-operator --destination dist

.PHONY: integration-ordered-bucket
integration-ordered-bucket:
	go run ./hack/integration --ordered-bucket

.PHONY: integration-maintenance
integration-maintenance:
	go run ./hack/integration --maintenance

# Fault injection: manager crash points, cordon + pod deletion, toxiproxy S3 latency/partition.
.PHONY: integration-faults
integration-faults:
	go run ./hack/integration --faults

# Two manager replicas; the leader is deleted while a contraction is issued.
.PHONY: integration-leader
integration-leader:
	go run ./hack/integration --leader-failover

# PersistentFleet lifecycle with ReadWriteOncePod claims on the per-node hostpath CSI driver.
.PHONY: integration-persistent-rwop
integration-persistent-rwop:
	go run ./hack/integration --persistent-lifecycle --rwop-csi

# External capacity mode: a real HPA drives spec.replicas through /scale.
.PHONY: integration-external
integration-external:
	go run ./hack/integration --external

# Documentation site (Astro + Starlight in site/). Pages are synced from docs/,
# config/, charts/ and cmd/ on every build; nothing under site/src/content is
# edited by hand except the start/, concepts/ and reference/ sections.
.PHONY: site site-dev
site:
	cd site && pnpm install --frozen-lockfile && pnpm build

site-dev:
	cd site && pnpm install --frozen-lockfile && pnpm dev
