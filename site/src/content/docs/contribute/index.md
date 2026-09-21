---
title: Contribute
description: Build the operator and maintain its strict lifecycle contracts and documentation.
---

Use the Go version in `go.mod`, a C compiler and Docker, then run:

```bash
make check
make test-envtest
make test-linux
```

See [CONTRIBUTING.md](https://github.com/ewhauser/celld-operator/blob/main/CONTRIBUTING.md)
for generated manifests, lint and integration commands. Read
[qualification](../qualification/) before extending test claims.

## Contracts

- [Architecture](../concepts/architecture/) and [current operations](../contracts/current-operation/).
- [celld wire protocol](../contracts/runtime-control-plane/) and [launcher supervision](../contracts/launcher-supervision/).
- [Disposable disks](../contracts/disposable-disks/) and [capacity policy](../contracts/capacity-policy/).
- [Current decision](../decisions/0022-celld-control-plane/).

The old private-S3 adapters, journal archive, EC2 fencing and disk-reuse harness
are removed. Their source and past reports remain in Git history, not in current
operating instructions.

## Documentation

Use Node 24–26 and the pnpm version in `site/package.json`:

```bash
cd site
pnpm install --frozen-lockfile
pnpm dev --background
```

Stop with `pnpm exec astro dev stop`. Run `pnpm build` to generate references,
build search and validate links. Edit user guides in `site/src/content/docs`;
implementation pages are synced from `docs/`, while API and chart references
come from generated CRDs and chart sources.

Validate commands against current code and distinguish implementation, local
tests, real cluster tests and AWS qualification. A local site build does not
publish documentation or validate the installation guide on EKS.
