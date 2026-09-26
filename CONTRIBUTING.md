# Contributing

## Development setup

Use the Go version declared in `go.mod` (currently 1.27.1). Race-enabled tests
also need a C compiler. The Make targets and tooling follow
[`cursor-controller`](https://github.com/ewhauser/cursor-controller), using its
local revision `bb37f8eb656f1622f170017464e990d88b855a5a` as the baseline.

```sh
make check       # Build, race-enabled tests, lint, and lint of the Linux build
make check-full  # check plus Linux container test runs and the envtest suite
make test-linux  # Controller tests cross-compiled and run on Linux (Docker)
make lint-linux  # golangci-lint analyzing GOOS=linux (catches Linux-only files)
make build       # Build packages and bin/celld-operator
make test        # go test -race ./...
make test-envtest # Reconciler tests against a real kube-apiserver/etcd (envtest)
make integration-faults # Disposable Kind: faults against an explicitly supplied compatible fork image
make vet         # Standalone go vet
make fmt         # Apply goimports through the pinned lint tool
make lint        # Full lint suite
make lint-new    # Report issues introduced relative to HEAD
make security-check # Workflow audit and security regression tests (GH_TOKEN enables online audits)
make vuln-check  # Go module integrity and vulnerability scan
```

golangci-lint v2.13.2 runs through `go run` with Go 1.27.1 and CGO disabled for
the lint tool. Application tests retain the race detector. No global linter
installation is required; the first invocation may download the tool.

The lint configuration is copied from cursor-controller, including context,
error handling, HTTP body closure, modernization, static analysis, and its
gocritic settings. Use standard-library `flag` for command options and return
errors to `main`; use `log/slog` when runtime logging is introduced. Keep
implementation packages under `internal/` as they are added.

`make test-linux` cross-compiles the controller test binary for
Linux and runs them in the pinned celld image with Docker, under `TZ=UTC`. Two
CI-only failures reached `main` before this target existed: a gocritic finding in
a Linux-only file and a `time.Time` comparison that only differs in a UTC zone.
Docker Desktop must share the repository path; the default `/Users` share works,
`/tmp` may not.

`make test-envtest` downloads pinned kube-apiserver and etcd binaries through
`setup-envtest` on first use and runs the `TestEnvtest*` cases in
`internal/controller`. Those tests cover what the fake client cannot: CRD CEL
admission and defaulting, real resourceVersion conflicts on the reservation and
workload CAS, and optimistic-lock merge patches. They skip silently when
`KUBEBUILDER_ASSETS` is unset, so `make test` needs no network.

The optional pre-commit hook runs `make lint-new`:

```sh
pre-commit install
```

The configuration is included, but hooks are not installed automatically.

## Documentation site

`site/` holds the Astro site published to GitHub Pages. Contract, decision and
qualification pages are synced from `docs/` at build time, and the API, chart,
sample and flag references are generated from `config/`, `charts/` and `cmd/`,
so edit those sources rather than the synced copies. User guides in
`site/src/content/docs/{start,configure,operate,troubleshoot,concepts,reference,contribute}`
are written by hand, except for the generated `reference/limitations.md` page.
`make site` builds it and fails on
broken internal links; `make site-dev` serves it locally.

Chart validation also needs Helm and the pinned Python dependencies used by CI:

```sh
python3 -m venv .qualification-venv
.qualification-venv/bin/pip install --require-hashes --only-binary=:all: -r hack/chart-requirements.txt
make chart-check
```

The runtime digest in `hack/runtime-image.txt` is the integration default. When
changing the recommended fork artifact, update `docs/runtime-versions.md`, the
website compatibility page, first-fleet manifest and native CLI release link
together. Keep older qualification receipts pinned to the artifacts they tested.

## Containers

```sh
make image VERSION=dev
docker run --rm ghcr.io/ewhauser/celld-operator:dev --version
```

The image uses a static Go binary and a nonroot distroless runtime, matching
cursor-controller. `make image` builds locally; it does not publish an image.

## Changes and validation

Keep changes focused. Add a failing regression test before fixing a bug and
update documentation when behavior changes. Run `make check` before opening a
pull request. For workflow changes, also run:

```sh
make security-check
```

Security reports belong in [private vulnerability reporting](SECURITY.md).
See [release security](docs/release-security.md) before changing publication,
workflow permissions, action pins, or dependency installation.

CI has separate build, race-test, and lint jobs using the same Make targets.
The kind integration suites need Docker and a three-node kind cluster. Every
pull request runs the `lifecycle` suite; the full matrix runs nightly, on demand from
the Integration workflow (`workflow_dispatch` with a suite name), and on a pull
request carrying the `integration` label. Run a suite locally with
`make integration` or `make integration-<suite>`.

The module pins controller-runtime and Kubernetes dependencies in `go.mod` and
`go.sum`. CI module caching is enabled. Run `make manifests-check` for generated
CRD/deepcopy reproducibility and `make integration` for a disposable kind cluster
with real runtime startup and enforced isolation; see [fleet API](docs/fleet-api.md).
Helm checks, local and Kind integration suites, and gated image/release publication
remain separate validation layers. The Make targets use the published fork digest
in `hack/runtime-image.txt`;
`CELLD_RUNTIME_IMAGE` overrides it with another qualified fork digest. No
stock-runtime fallback is permitted.
Describe exactly which local, cluster, and AWS checks ran; a passing baseline
does not qualify celld durability or scaling behavior.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License, Version 2.0](LICENSE), as described in Section 5 of that
license.

## Optional Gateway API validation

Normal `make test-envtest` covers Ingress admission, routing edits/removal and
operation without Gateway CRDs. To also exercise HTTPRoute admission/defaulting
and current-generation acceptance with the standard Gateway API schema:

```sh
curl -fsSL https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v1.4.0/config/crd/standard/gateway.networking.k8s.io_httproutes.yaml -o /tmp/celld-httproute-v1.4.0.yaml
CELLD_GATEWAY_CRD=/tmp/celld-httproute-v1.4.0.yaml \
KUBEBUILDER_ASSETS="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1 use 1.37.0 -p path)" \
go test -race ./internal/controller -run '^TestEnvtestGatewayRouting$' -count=1
```

This runs a disposable API server, not a Gateway controller or CNI data plane.
Verify actual HTTP/TLS traffic and NetworkPolicy enforcement with the selected
edge implementation in your deployment environment.
