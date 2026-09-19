VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/ewhauser/celld-operator
LDFLAGS := -s -w -X main.version=$(VERSION)
GO_PACKAGES := ./...
RACE ?= -race
GOLANGCI_LINT_VERSION ?= v2.13.2
GOLANGCI_LINT := GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: check build test vet fmt lint lint-new image clean

check: build test lint

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
	python3 hack/integration/run.py

.PHONY: integration-bucket
integration-bucket:
	python3 hack/integration/run.py --bucket-lifecycle

.PHONY: integration-persistent
integration-persistent:
	python3 hack/integration/run.py --persistent-lifecycle

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
	python3 hack/integration/run.py --ordered-bucket

.PHONY: integration-maintenance
integration-maintenance:
	python3 hack/integration/run.py --maintenance

# Fault injection: manager crash points, cordon + pod deletion, toxiproxy S3 latency/partition.
.PHONY: integration-faults
integration-faults:
	python3 hack/integration/run.py --faults

# PersistentFleet lifecycle with ReadWriteOncePod claims on the per-node hostpath CSI driver.
.PHONY: integration-persistent-rwop
integration-persistent-rwop:
	python3 hack/integration/run.py --persistent-lifecycle --rwop-csi
