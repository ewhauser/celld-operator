---
title: Compatibility
description: Supported Kubernetes, celld runtime, journal and toolchain versions, and the transitions between them that are and are not implemented.
---

## Platform

| Component | Supported | Source |
| --- | --- | --- |
| Kubernetes | 1.31 or newer, IPv4 Pod networking | [Fleet API](../../contracts/fleet-api/); the chart's `kubeVersion` floor is `>=1.30.0-0` |
| Cloud | AWS EKS with S3 and EBS | [ADR 0003](../../decisions/0003-aws-platform-and-provisioning/) |
| CNI | Any that enforces NetworkPolicy, verified by you | [Operations](../../contracts/operations/) |
| Storage (PersistentFleet) | EBS CSI, gp2 or gp3, `ReadWriteOncePod`, `Retain`, `WaitForFirstConsumer` | [PersistentFleet lifecycle](../../contracts/persistent-fleet-lifecycle/) |
| EC2 fencing | Standard commercial regional endpoints only; dedicated instances with the documented tags | [EC2 fencing](../../contracts/infrastructure-fencing/) |
| Metrics Server | Required only for capacity policy | [Capacity policy](../../contracts/capacity-policy/) |
| Prometheus Operator | Optional, for ServiceMonitor and PrometheusRule | [ADR 0006](../../decisions/0006-metrics-dependencies/) |

Other Kubernetes distributions and S3-compatible stores are outside qualification. The disposable kind harness with MinIO exists for the operator's own tests and proves nothing about AWS.

## celld runtime

The operator recognises exactly two multi-platform release index digests. Mutable tags, per-architecture digests and other releases are rejected.

| Release | Commit | Image |
| --- | --- | --- |
| v0.4.1 | `10cb1303dac710dcb3b557e318e08c855261f68b` | `ghcr.io/denoland/celld@sha256:ce8bbc3c26a16c9ee00e3ce0501f36bfea2663b5af8285a08fc16a54568060a5` |
| v0.5.0 (default) | `12d5b6333fe52717325addcfe1e99e9fd4f77bcd` | `ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8` |

| Transition | Status |
| --- | --- |
| v0.4.1 to v0.5.0, PersistentFleet | Implemented with whole-fleet coordinated downtime, sealed logs and retained disks |
| v0.5.0 to v0.4.1 | Rejected; no reverse storage-format contract exists |
| Any transition, Bucket | Not implemented |
| Any other digest | `UnsupportedTransition`, retained as a blocked request |
| Creating a v0.4.1 fleet | PersistentFleet with the launcher only |

Details, including the version-specific environment differences, are in [runtime versions](../../contracts/runtime-versions/).

## Journal

| Version | Status |
| --- | --- |
| 8 | Current. Written on every durable update. |
| 1 to 7 | Read conservatively and rewritten as 8 on the next write. |
| Newer than the binary knows | Refused. The controller does not start against it. |

Downgrading the operator after the journal advanced is not a rollback procedure. See [journal archives](../../contracts/journal-archives/).

## Toolchain

| Tool | Version |
| --- | --- |
| Go | 1.27.1 (`go.mod`) |
| controller-runtime | v0.25.0 |
| Kubernetes client libraries | v0.37.0 |
| controller-gen | v0.20.1 |
| golangci-lint | v2.13.2 |
| Helm (CI) | v4.2.4 |
| envtest Kubernetes | 1.37.0 |
| Node (this site) | 24 or newer |

## Migrations that do not exist

- Converting an unwrapped or `ReadWriteOnce` PersistentFleet to the launcher and `ReadWriteOncePod`.
- Rolling out the stronger Pod template with hostname anti-affinity to fleets provisioned before it. Those report drift instead.
- Freeing a bucket reservation or reattaching a retained disk to a different fleet identity.
- Recovering an all-stopped PersistentFleet whose logs did not seal.

Each is described with the runtime reason in [runtime dependencies](../../contracts/runtime-dependencies/).
