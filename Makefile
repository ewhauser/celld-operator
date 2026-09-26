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
RUNTIME_IMAGE ?= $(shell cat hack/runtime-image.txt)
export CELLD_RUNTIME_IMAGE ?= $(RUNTIME_IMAGE)
HOST_GOARCH := $(shell go env GOARCH)
ACTIONLINT_VERSION := v1.7.12
GOVULNCHECK_VERSION := v1.8.0
SECURITY_PYTHON := .security-venv/bin/python
SECURITY_ENV_STAMP := .security-venv/.installed

.PHONY: check check-full build test vet fmt lint lint-linux lint-new test-linux image clean

# Linux-only files (build tags) are invisible to a
# macOS lint run; lint-linux analyzes the Linux build so they cannot reach CI red.
check: build test lint lint-linux

# Everything check does plus the suites that need Docker or envtest binaries.
check-full: check test-linux test-envtest

# Online zizmor audits include impostor-commit and ref-version-mismatch when
# GH_TOKEN is available. CI supplies only its read-only, job-scoped token.
.PHONY: security-check vuln-check
$(SECURITY_ENV_STAMP): hack/security-requirements.txt
	python3 -m venv .security-venv
	$(SECURITY_PYTHON) -m pip install --require-hashes --only-binary=:all: -r $<
	touch $@

security-check: $(SECURITY_ENV_STAMP)
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	.security-venv/bin/zizmor --persona pedantic --strict-collection --config .github/zizmor.yaml .github/workflows
	$(SECURITY_PYTHON) -m unittest discover -s hack -p 'test_*.py'

vuln-check:
	go mod verify
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

build:
	go build -o /dev/null $(GO_PACKAGES)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/celld-operator ./cmd/celld-operator

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

# Cross-compile the controller tests and run them on Linux in the pinned
# runtime image.
# No race detector: that needs cgo. Requires Docker.
test-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_GOARCH) go test -c -o bin/tests/controller.test ./internal/controller
	docker run --rm --platform linux/$(HOST_GOARCH) -e TZ=UTC -v $(CURDIR)/bin/tests:/t:ro -v $(CURDIR):/src:ro -w /src/internal/controller --entrypoint /t/controller.test $(RUNTIME_IMAGE) -test.count=1

lint-new:
	$(GOLANGCI_LINT) run --new-from-rev=HEAD ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
.PHONY: generate manifests-check integration
generate:
	$(CONTROLLER_GEN) object crd paths=./api/... output:crd:artifacts:config=config/crd

manifests-check:
	python3 hack/check-generated.py

# Actual Kind workloads, strict fork runtime and hostpath CSI RWOP/Delete storage.
.PHONY: integration-store-images integration integration-lifecycle integration-maintenance integration-faults integration-external integration-extended
integration-store-images:
	bash hack/build-integration-store.sh
integration: integration-store-images
	go run ./hack/integration --suite all
integration-lifecycle: integration-store-images
	go run ./hack/integration --suite lifecycle
integration-maintenance: integration-store-images
	go run ./hack/integration --suite maintenance
integration-faults: integration-store-images
	go run ./hack/integration --suite faults
integration-external: integration-store-images
	go run ./hack/integration --suite external
integration-extended: integration-store-images
	go run ./hack/integration --suite extended

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

# Documentation site (Astro + Starlight in site/). Pages are synced from docs/,
# config/, charts/ and cmd/ on every build; nothing under site/src/content is
# edited by hand except the start/, concepts/ and reference/ sections.
.PHONY: site site-dev
site:
	cd site && pnpm install --frozen-lockfile && pnpm build

site-dev:
	cd site && pnpm install --frozen-lockfile && pnpm dev

.PHONY: integration-previews
# CELLD_PREVIEW_IMAGE must contain both preview runtime and CLI support.
integration-previews: integration-store-images
	go run ./hack/integration --suite previews
