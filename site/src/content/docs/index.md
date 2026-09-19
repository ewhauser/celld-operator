---
title: celld operator documentation
description: Experimental Kubernetes fleet operator for celld on EKS, S3 and EBS.
tableOfContents: false
---

celld operator is a Go controller that provisions and scales fleets of the [celld](https://github.com/denoland/celld) runtime on Kubernetes. It manages a namespaced `CelldFleet` resource in two profiles, `Bucket` and `PersistentFleet`, records every lifecycle operation in a durable journal, and executes contraction, restart, migration and deletion only on positive evidence. celld keeps ownership of cells, leases, replication and recovery.

:::caution[Experimental]
Every path is experimental. Fleets must declare `qualification: Experimental`, `ProductionQualified` is always false, and nothing has been qualified on AWS. [Implementation status](./contracts/critical-features/) is the authoritative list of what executes and which release gates remain open.
:::

Source: [github.com/ewhauser/celld-operator](https://github.com/ewhauser/celld-operator). The Contracts, Qualification evidence and Design decisions sections are synced from the repository's `docs/` directory at build time; the API, chart and flag references are generated from the CRDs, chart and binary.

## Start here

- [What celld operator is](./start/overview/): scope, what it does and deliberately does not do.
- [Install](./start/install/): prerequisites, CRDs, chart, per-namespace RBAC, NetworkPolicy attestation.
- [Your first fleet](./start/first-fleet/): adapt a sample, apply it, read the conditions.
- [Verify and observe](./start/verify/): status fields, Events, metrics, and which signals are not evidence.

## Concepts

- [Architecture](./concepts/architecture/): owned resources, reconciliation flow, external inputs.
- [Fleet profiles](./concepts/profiles/): Bucket and PersistentFleet side by side.
- [Lifecycle journal](./concepts/lifecycle-journal/): the storage reservation, its invariants and versioning.
- [Safety model](./concepts/safety-model/): accepted evidence, refused inferences, blocker reasons.

## Contracts

The repository's behavioural contracts, unchanged: [fleet API](./contracts/fleet-api/), [operations](./contracts/operations/), [capacity policy](./contracts/capacity-policy/), [maintenance execution](./contracts/maintenance-execution/), [Bucket scale-in](./contracts/bucket-scale-in/), [Ordered Bucket](./contracts/ordered-bucket/), [Bucket migration](./contracts/bucket-migration/), [PersistentFleet lifecycle](./contracts/persistent-fleet-lifecycle/), [EC2 fencing](./contracts/infrastructure-fencing/), [runtime versions](./contracts/runtime-versions/), [journal archives](./contracts/journal-archives/), [runtime dependencies](./contracts/runtime-dependencies/).

## Reference

- [CelldFleet API](./api/celldfleet/) and [CelldStorageReservation API](./api/celldstoragereservation/), generated from the CRDs.
- [Sample manifests](./api/samples/), [Helm chart values](./api/helm-values/), [operator flags](./api/operator-flags/).
- [Conditions and blockers](./reference/conditions/), [compatibility](./reference/compatibility/), [security boundaries](./reference/security-boundaries/).

## Evidence and decisions

- [Qualification evidence](./qualification/): recorded local runs, measured findings and the open gates, including the [EKS smoke suite plan](./qualification/eks-smoke-plan/).
- [Design decisions](./decisions/): the seventeen architecture decision records.
- Historical investigations: [runtime qualification](./history/runtime-qualification/), [shutdown evidence](./history/shutdown-evidence/), [S3 recovery evidence](./history/s3-recovery-evidence/), [shared lifecycle safety](./history/shared-lifecycle-safety/), [PersistentFleet implementation gap](./history/persistent-fleet-implementation-gap/), [pre-release review](./history/pre-release-review/).
