# celld-operator

Kubernetes fleet operator for celld, targeting AWS EKS with S3 and EBS.

This repository is an experimental operator: a namespaced fleet API, reservation-arbitrated
provisioning for Bucket and PersistentFleet profiles, a durable lifecycle journal, manual
scale-out and contraction in both profiles (PersistentFleet through a trusted launcher),
same-version restarts, retained deletion, one directional runtime upgrade, and an
optional capacity policy. Every path is experimental. Automatic contraction executes only
in the local test fixture; production automatic contraction, EKS/S3/EBS behavior and node
failure remain release gates. The authoritative status is
[docs/critical-features.md](docs/critical-features.md).

## Development

Install Go 1.27.1 and a C compiler for race-enabled tests, then run:

```sh
make check
```

This builds all packages and `bin/celld-operator`, runs `go test -race`, and runs
the pinned golangci-lint suite. The command supports `--help` and `--version`; starting it runs the controller
against the explicitly configured Kubernetes environment. Provisioning is blocked
until CNI enforcement is attested. Isolated local integration and qualification
harnesses are available; they do not enable production scaling.

See [CONTRIBUTING.md](CONTRIBUTING.md) for formatting, linting, hooks, and container
builds, and [docs/](docs/README.md) for the architecture and qualification work.
The same documentation is published at
[ewhauser.github.io/celld-operator](https://ewhauser.github.io/celld-operator/);
`make site` builds it locally from `site/`.

Runtime qualification: [measured findings and release gates](docs/qualification/README.md), [local harness](hack/qualification/README.md).

## Where to read

- [Fleet API, installation and safety boundaries](docs/fleet-api.md).
- [Current implementation checklist and remaining gates](docs/critical-features.md).
- Contracts: [Bucket contraction](docs/bucket-scale-in.md),
  [PersistentFleet launcher lifecycle](docs/persistent-fleet-lifecycle.md),
  [maintenance execution](docs/maintenance-execution.md),
  [runtime versions](docs/runtime-versions.md), [capacity policy](docs/capacity-policy.md).
- [Architecture decisions](docs/decisions/README.md) and
  [qualification evidence](docs/qualification/README.md), including the
  [fault-injection suite](docs/qualification/faults/README.md).

Test layers: `make test` (unit, fake client), `make test-envtest` (real API server),
`make integration` and its `integration-*` variants (disposable kind cluster).
