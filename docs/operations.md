# Installation and operations

Use the task-oriented [installation guide](../site/src/content/docs/start/install.mdx),
[scaling guide](../site/src/content/docs/operate/scaling.md),
[maintenance guide](../site/src/content/docs/operate/restart.md) and
[deletion guide](../site/src/content/docs/operate/deletion.md).

Both Bucket and PersistentFleet need a compatible digest-pinned fork runtime and
launcher. The operator has Kubernetes credentials only; celld receives the runtime
bucket identity. The manager does not read S3 recovery metadata or terminate EC2
instances. The fleet namespace Role includes guarded PVC deletion; storage
finalizers remain under Kubernetes and CSI control.

For implementation details use [current operations](current-operation.md),
[launcher supervision](launcher-supervision.md) and [typed control plane](runtime-control-plane.md).
A blocked operation must preserve current authority and storage. Removing an
annotation, claim finalizer or fleet finalizer is not a recovery procedure.

## Build and validate

`make check` builds the Go packages, runs race tests and checks native/Linux lint.
`make test-envtest` exercises the real API server's admission and resource-version
conflicts. `make test-linux` executes Linux process-lock tests with Docker.
`make chart-check` and `make manifests-check` verify shipped configuration.
The [test records](qualification/README.md) document strict-runtime integration
and the environments exercised.

## Releases

Publish an immutable operator image and use its digest for both the manager and
launcher. Verify the exact compatible celld fork image separately; native binary
artifacts do not identify a container digest. Chart packaging and registry
verification are release workflow responsibilities. A local build is not a published release.

Use the [release security guide](release-security.md) for the approved publishing
workflow and exact artifact verification commands.
