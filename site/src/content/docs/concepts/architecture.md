---
title: Architecture
description: celld owns data safety; the launcher and controller prove process and infrastructure effects.
---

A CelldFleet describes one independent celld cluster. The operator creates its
Kubernetes resources and coordinates lifecycle requests. Each fleet has a
dedicated bucket and a permanent storage reservation bound to its UID.

![The operator obtains celld safety through the launcher and records one current operation in Kubernetes. celld owns all S3 data access.](../../../assets/architecture.svg)

| Component | Responsibility |
| --- | --- |
| celld | Application execution, replication, tiering, recovery and strict shutdown data safety. |
| Launcher | Capture the exact runtime result, terminate its child, release the inherited lock and deny restart. |
| Operator | Placement, capacity observation, bounded current operation and guarded workload/storage effects. |
| Kubernetes and CSI | Scheduling, resource versions, storage protection, attachment and volume deletion. |

Both profiles use the launcher. Bucket uses a Deployment or Ordered StatefulSet
with temporary disk. PersistentFleet uses a StatefulSet with dynamically
provisioned CSI disks. All runtime/recovery members need the compatible fork.

The operator creates application and headless Services, NetworkPolicies, a
PodDisruptionBudget, a launcher Secret and storage reservations. It has no S3
client or EC2 termination path. You supply buckets, runtime IAM roles, nodes,
CSI, ingress, TLS and DNS. PersistentFleet advertises stable per-Pod DNS through
its headless Service, including before readiness, so celld can reach retained
follower logs after Pod IPs change.

Capacity recommendations and manual requests enter the same
[current-operation executor](../current-operation/). The reservation records
intent before issuance and proof before effects. Status is reconstructed from
current authority; clearing it cannot authorize deletion.

Read the [strict protocol](../../contracts/runtime-control-plane/),
[launcher contract](../../contracts/launcher-supervision/) and
[architecture decision](../../decisions/0022-celld-control-plane/) for details.
