# celld-operator

Kubernetes fleet operator for celld, targeting AWS EKS with S3 and EBS.

This repository currently contains architecture decisions, runtime investigations,
and the Go development baseline. Fleet reconciliation is not implemented.

## Development

Install Go 1.27.1 and a C compiler for race-enabled tests, then run:

```sh
make check
```

This builds all packages and `bin/celld-operator`, runs `go test -race`, and runs
the pinned golangci-lint suite. The command currently supports `--help` and
`--version`; starting it without either flag reports that reconciliation is not
implemented. A versioned runtime adapter, fixture tests, and an isolated local
qualification harness are available; they do not enable production scaling.

See [CONTRIBUTING.md](CONTRIBUTING.md) for formatting, linting, hooks, and container
builds, and [docs/](docs/README.md) for the architecture and qualification work.

Runtime qualification: [measured findings and release gates](docs/qualification/README.md), [local harness](hack/qualification/README.md).
