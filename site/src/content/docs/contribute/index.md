---
title: Contribute to celld operator
description: Build the operator, improve its documentation, and find architecture and test records.
---

Start here to change the operator or its documentation. If you are deploying a fleet,
use [Get started](../start/overview/) or the [operation guides](../operate/scaling/).

## Build and test

Clone the [repository](https://github.com/ewhauser/celld-operator), install the Go
version in `go.mod` and a C compiler, then run from the repository root:

```bash
make check
```

This builds the binaries and runs race-enabled tests and lint. Follow
[CONTRIBUTING.md](https://github.com/ewhauser/celld-operator/blob/main/CONTRIBUTING.md)
for generated manifests, container images, and integration tests. Integration tests
create disposable local clusters; local results do not validate EKS or EBS behavior.

## Work on the documentation

With Node 24–26 and the pnpm version in `site/package.json`:

```bash
cd site
pnpm install --frozen-lockfile
pnpm dev --background
```

Open `http://localhost:4321/celld-operator/`. Stop the server with
`pnpm exec astro dev stop`. Before submitting a change, run `pnpm build`; it
generates references, builds search, and checks internal links.

User guides live in `site/src/content/docs/`. Write each guide around a task:
its prerequisites, commands, expected results, completion checks, and ways to
resolve failures. Check commands against the chart, API, and controller. Describe
current behavior separately from what was tested.

API fields come from the Go API comments and generated CRDs; update those sources
and run `make generate` and `make chart-sync`. The current capability matrix comes
from `docs/critical-features.md`. Engineering records below are copied into the site
at build time. Edit their repository sources, not the ignored generated copies.

## Understand the implementation

- [Architecture](../concepts/architecture/): components, workloads, and data flow.
- [How operations resume](../concepts/lifecycle-journal/): persisted operations and retained recovery records.
- [How removal is checked](../concepts/safety-model/): why uncertain operations stop for investigation.
- [Implementation details](../contracts/fleet-api/): controller behavior and links to individual protocols.

## Design and test archive

[Design records](../decisions/) explain past decisions.
[Test reports](../qualification/) describe the versions and environments used in
each run. They are historical evidence, not installation instructions or a live
support matrix. These records are excluded from user search so old results cannot
masquerade as current guidance; use the expandable contributor navigation to browse them.

Historical investigations include [runtime behavior](../history/runtime-qualification/),
[shutdown](../history/shutdown-evidence/), [S3 recovery records](../history/s3-recovery-evidence/),
[shared lifecycle behavior](../history/shared-lifecycle-safety/),
[persistent storage](../history/persistent-fleet-implementation-gap/), and the
[early implementation review](../history/pre-release-review/).

For current behavior and outstanding validation, use
[Capabilities and limitations](../reference/limitations/).
