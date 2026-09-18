# celld-operator

Kubernetes fleet operator for celld, targeting AWS EKS with S3 and EBS.

This repository includes an experimental namespaced fleet API, initial infrastructure
reconciliation, journaled manual scale-out and Bucket scale-in, and a version-pinned runtime evidence adapter. Production lifecycle
automation remains blocked pending qualification.

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

Runtime qualification: [measured findings and release gates](docs/qualification/README.md), [local harness](hack/qualification/README.md).

## Experimental infrastructure controller

Step 2 defines the namespaced CelldFleet API and initial provisioning for both
profiles. See [API, examples and safety boundaries](docs/fleet-api.md). Production
qualification and PersistentFleet contraction remain blocked. [Bucket logical membership contraction](docs/bucket-scale-in.md) supports experimental manual requests; production automatic removal remains release-gated. Step 3 adds a durable operation
journal and fault-tested contraction engine behind explicit qualification gates;
see [lifecycle validation](docs/qualification/lifecycle/README.md).

Step 4 adds [optional capacity policy](docs/capacity-policy.md). Step 5 adds
[coordinated maintenance](docs/decisions/0014-coordinated-maintenance.md): durable
blocked upgrade/restart requests, pause/resume fencing and retained deletion.
No runtime transition, rollback, planned restart or final shutdown is qualified.
