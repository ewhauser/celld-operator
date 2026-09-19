# Contributing

## Development setup

Use the Go version declared in `go.mod` (currently 1.27.1). Race-enabled tests
also need a C compiler. The Make targets and tooling follow
[`cursor-controller`](https://github.com/ewhauser/cursor-controller), using its
local revision `bb37f8eb656f1622f170017464e990d88b855a5a` as the baseline.

```sh
make check       # Build, race-enabled tests, and lint
make build       # Build packages and bin/celld-operator
make test        # go test -race ./...
make test-envtest # Reconciler and journal tests against a real kube-apiserver/etcd (envtest)
make integration-faults # Disposable kind: manager crash points, node loss, toxiproxy S3 latency/partition
make vet         # Standalone go vet
make fmt         # Apply goimports through the pinned lint tool
make lint        # Full lint suite
make lint-new    # Report issues introduced relative to HEAD
```

golangci-lint v2.13.2 runs through `go run` with Go 1.27.1 and CGO disabled for
the lint tool. Application tests retain the race detector. No global linter
installation is required; the first invocation may download the tool.

The lint configuration is copied from cursor-controller, including context,
error handling, HTTP body closure, modernization, static analysis, and its
gocritic settings. Use standard-library `flag` for command options and return
errors to `main`; use `log/slog` when runtime logging is introduced. Keep
implementation packages under `internal/` as they are added.

`make test-envtest` downloads pinned kube-apiserver and etcd binaries through
`setup-envtest` on first use and runs the `TestEnvtest*` cases in
`internal/controller`. Those tests cover what the fake client cannot: CRD CEL
admission and defaulting, real resourceVersion conflicts on the journal and
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
so edit those sources rather than the synced copies. Only `site/src/content/docs/start`,
`concepts` and `reference` are written by hand. `make site` builds it and fails on
broken internal links; `make site-dev` serves it locally.

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
actionlint .github/workflows/ci.yaml
```

CI has separate build, race-test, and lint jobs using the same Make targets.
The kind integration suites run nightly and on demand from the Integration
workflow (`workflow_dispatch` with a suite name, or the `integration` label on a
pull request); each suite is a disposable three-node cluster and takes 15 to 25
minutes.
The test job also runs `make qualification-replay` and `make qualification-test`;
see the [qualification harness](hack/qualification/README.md) for Python setup
and the separately invoked local Docker experiments.
GitHub Actions are pinned to immutable commits and workflow permissions are
read-only. Renovate follows the source project's security-only update policy,
three-day release cooldown, and grouped, digest-pinned GitHub Actions updates.

The module pins controller-runtime and Kubernetes dependencies in `go.mod` and
`go.sum`. CI module caching is enabled. Run `make manifests-check` for generated
CRD/deepcopy reproducibility and `make integration` for a disposable kind cluster
with real runtime startup and enforced isolation; see [fleet API](docs/fleet-api.md).
Helm checks, local and kind integration suites, and gated image/release
publication should be added alongside the corresponding deployable operator.
Describe exactly which local, cluster, and AWS checks ran; a passing baseline
does not qualify celld durability or scaling behavior.
