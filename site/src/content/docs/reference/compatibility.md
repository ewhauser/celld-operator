---
title: Compatibility
description: Kubernetes requirements, accepted celld runtime images, and upgrade restrictions.
---

Use this page to check your cluster and choose an accepted runtime image. All
paths remain experimental; see [capabilities and limitations](../limitations/).

## Platform

| Component | Requirement | Setup |
| --- | --- | --- |
| Kubernetes | 1.31 or newer, IPv4 Pod networking | The chart enforces the same 1.31 minimum. |
| Cloud | AWS EKS with S3 and EBS | [AWS permissions](../../configure/aws/) |
| CNI | Any that enforces NetworkPolicy, verified by you | [Networking](../../configure/networking/) |
| Storage (PersistentFleet) | EBS CSI, gp2 or gp3, `ReadWriteOncePod`, `Retain`, `WaitForFirstConsumer` | [Storage](../../configure/storage/) |
| EC2 fencing | Standard commercial regional endpoints only; dedicated instances with the documented tags | [EC2 fencing](../../contracts/infrastructure-fencing/) |
| Metrics Server | Required only for capacity policy | [Capacity policy](../../operate/capacity/) |
| Prometheus Operator | Optional, for ServiceMonitor and PrometheusRule | [Monitoring](../../operate/monitoring/) |

Other Kubernetes distributions and S3-compatible stores are outside qualification. The disposable kind harness uses MinIO for development tests; real AWS behavior still requires separate testing.

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

Follow [Upgrade the runtime](../../operate/upgrade-runtime/) for the supported procedure. An accepted image is not permission to perform a rolling update.

## Journal

| Version | Status |
| --- | --- |
| 8 | Current. Written on every durable update. |
| 1 to 7 | Read conservatively and rewritten as 8 on the next write. |
| Newer than the binary knows | The affected fleet is blocked; the controller does not interpret the newer journal. |

Downgrading the operator after the journal advanced is not a rollback procedure. See [journal archives](../../contracts/journal-archives/).

## Unsupported migrations

- Converting an unwrapped or `ReadWriteOnce` PersistentFleet to the launcher and `ReadWriteOncePod`.
- Rolling out the stronger Pod template with hostname anti-affinity to fleets provisioned before it. Those report drift instead.
- Freeing a bucket reservation or reattaching a retained disk to a different fleet identity.
- Recovering an all-stopped PersistentFleet whose logs did not seal.

See [capabilities and limitations](../limitations/) and [blocked operations](../../troubleshoot/lifecycle/) before attempting recovery.
