---
title: What celld operator is
description: A namespaced Kubernetes operator that provisions and scales celld fleets on EKS while keeping every unqualified action blocked and visible.
sidebar:
  order: 1
---

celld operator is a Go controller, built on controller-runtime, that manages fleets of the [celld](https://github.com/denoland/celld) runtime on Kubernetes. You describe a fleet with a namespaced `CelldFleet` resource; the operator creates the workload, Services, NetworkPolicy, disruption budget and, for the persistent profile, retained EBS-backed claims. celld keeps ownership of cells, leases, replication and recovery. The operator owns Kubernetes resources and the lifecycle gates around them.

:::caution[Experimental]
Every path in this project is experimental. The API requires `qualification: Experimental`, conditions report `ProductionQualified=False` throughout, and nothing has been qualified on AWS. The [implementation status](../../contracts/critical-features/) page is the authoritative list of what executes today and what remains a release gate.
:::

## What it does today

- **Provisions two profiles.** `Bucket` fleets run on disk-backed ephemeral storage with S3 as the durability basis. `PersistentFleet` fleets run as a StatefulSet on retained EBS volumes. See [fleet profiles](../../concepts/profiles/).
- **Journals every lifecycle operation.** Scale-out, contraction, restart, migration and deletion are recorded on a cluster-scoped `CelldStorageReservation` before any effect is issued, so they survive leader failover and controller restarts. See [lifecycle journal](../../concepts/lifecycle-journal/).
- **Executes manual contraction with evidence.** Bucket removals complete on positively observed expired leases read from S3. PersistentFleet removals go through a trusted launcher that produces authenticated stop receipts. See [safety model](../../concepts/safety-model/).
- **Recommends or performs bounded scale-out.** An optional capacity policy watches celld's private state and Metrics Server, journals decisions, and can add replicas within explicit bounds. See [capacity policy](../../contracts/capacity-policy/).
- **Runs coordinated maintenance.** Same-version rolling restarts, retained deletion, a one-way Bucket layout migration and one directional runtime upgrade with explicit downtime. See [maintenance execution](../../contracts/maintenance-execution/).

## What it deliberately does not do

- Provision AWS resources. Buckets, IAM roles, node capacity, the EBS CSI driver and StorageClasses are yours. See [ADR 0003](../../decisions/0003-aws-platform-and-provisioning/).
- Expose public traffic. Each fleet gets a ClusterIP Service; ingress, TLS and DNS stay outside. See [ADR 0009](../../decisions/0009-service-and-ingress-boundary/).
- Modify celld. The operator pins exact released image digests and injects a launcher beside them. See [ADR 0007](../../decisions/0007-runtime-compatibility/).
- Infer safety. Pod absence, lease expiry inferred from time, a successful exit code or a retained PVC are never treated as proof that a writer stopped. See [ADR 0008](../../decisions/0008-recovery-evidence-and-conservative-removal/).
- Clean up. Deletion retains the workload identity, PVCs, bucket reservation and journal behind a finalizer. Freeing a bucket or disk is future, separately qualified work.

## How the documentation is organised

| Section | What it holds |
| --- | --- |
| Start here | Installation, a first fleet, and how to read status. Written for the site. |
| Concepts | Architecture, profiles, the journal and the safety model. Written for the site. |
| Contracts | The repository's behavioural contracts, published unchanged from `docs/`. |
| Reference | Generated API, chart and flag references plus conditions, compatibility and security boundaries. |
| Qualification evidence | Recorded local runs with their measured findings and open gates, from `docs/qualification/`. |
| Design decisions | The architecture decision records, from `docs/decisions/`. |
| Historical investigations | Step records kept for their reasoning; check the contracts for current behaviour. |

Contract, qualification and decision pages are synced from the repository at build time, so the site never drifts from what is committed. Each page's "Edit page" link opens the source file.
