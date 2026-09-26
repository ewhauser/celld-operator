# celld-operator

Kubernetes fleet operator for celld, targeting AWS EKS with S3 and EBS.

Define a fleet with a `CelldFleet` manifest. The operator creates its workloads,
Services and network policies, then coordinates scaling and maintenance. You
supply the cluster, S3 bucket and AWS identities; PersistentFleet also uses
retained CSI disks and changes one member at a time.

> [!WARNING]
> If you use celld with persistent local storage (`PersistentFleet`), you must
> use the [ewhauser/celld fork](https://github.com/ewhauser/celld). The operator
> relies on its node-log state to pace changes and decide when a local disk may
> be deleted; stock upstream celld does not provide that state. All runtime and
> recovery nodes must use a compatible fork. The fork is also required for
> `Bucket` fleets. See [compatibility](site/src/content/docs/reference/compatibility.md)
> for the required release and image digest.

## Use the operator

- [Install](site/src/content/docs/start/install.mdx), [create a fleet](site/src/content/docs/start/first-fleet.mdx), and [run an application](site/src/content/docs/start/first-application.md).
- [Choose a storage profile](site/src/content/docs/configure/profiles.md) and [configure AWS permissions](site/src/content/docs/configure/aws.md).
- [Expose HTTP traffic](site/src/content/docs/configure/networking.md) through an existing Gateway or Ingress controller.
- [Create isolated application previews](docs/previews.md) on shared pools with small runtimes, unique URLs and automatic expiry.
- [Scale](site/src/content/docs/operate/scaling.md), [restart](site/src/content/docs/operate/restart.md), [monitor](site/src/content/docs/operate/monitoring.md), and [troubleshoot](site/src/content/docs/troubleshoot/index.md).

The [documentation website](https://ewhauser.github.io/celld-operator/) provides
the full user guide and generated reference. `make site` builds it locally.

## Development

Install Go 1.27.1 and a C compiler for race-enabled tests, then run:

```sh
make check
```

This builds all packages and `bin/celld-operator`, runs `go test -race`, and runs
the pinned golangci-lint suite. The command supports `--help` and `--version`; starting it runs the controller
against the explicitly configured Kubernetes environment. Provisioning is blocked
until CNI enforcement is attested. Local integration suites are available for regression testing.

See [CONTRIBUTING.md](CONTRIBUTING.md) for formatting, linting, hooks, and container
builds, and [docs/](docs/README.md) for architecture and test evidence.
Test evidence and boundaries: [qualification records](docs/qualification/README.md). Both profiles require an explicit compatible fork runtime digest and run celld directly; PersistentFleet reads celld's node-log state. The controller has no S3 or EC2 client.

## Contributor references

See [docs/](docs/README.md) for implementation details, [design decisions](docs/decisions/README.md),
and [recorded test evidence](docs/qualification/README.md).

Test layers: `make test` (unit, fake client), `make test-envtest` (real API server),
`make integration` and its variants (disposable kind cluster). For the operator-backed
preview CLI and multi-object cloning, see [`make integration-previews`](docs/preview-integration.md).

## License

celld-operator is licensed under the [Apache License, Version 2.0](LICENSE).
