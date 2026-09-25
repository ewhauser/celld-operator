---
title: Architecture
description: celld owns data safety; the launcher and controller prove process and infrastructure effects.
---

A CelldFleet describes one independent celld cluster. The operator creates its
Kubernetes resources and coordinates lifecycle requests. Each fleet has a
dedicated bucket and a permanent storage reservation bound to its UID.

![For PersistentFleet, the operator obtains celld safety through the launcher and records one current operation in Kubernetes. celld owns all S3 data access.](../../../assets/architecture.svg)

| Component | Responsibility |
| --- | --- |
| celld | Application execution, replication, tiering, recovery and strict shutdown data safety. |
| Launcher | PersistentFleet only: capture the exact runtime result, terminate its child, release the inherited lock and deny restart. |
| Operator | Placement, capacity observation, bounded current operation (PersistentFleet), one-member Bucket rollouts and guarded workload/storage effects. |
| Kubernetes and CSI | Scheduling, resource versions, storage protection, attachment and volume deletion. |

Bucket uses a Deployment or Ordered StatefulSet with temporary disk and runs
celld directly with `CELLD_DURABILITY=bucket`. PersistentFleet uses a StatefulSet
with dynamically provisioned CSI disks under the launcher. All runtime/recovery members need the compatible fork.

The operator creates application and headless Services, NetworkPolicies, a
PodDisruptionBudget, storage reservations and, for PersistentFleet, a launcher
Secret. It has no S3
client or EC2 termination path. You supply buckets, runtime IAM roles, nodes,
CSI, ingress, TLS and DNS. PersistentFleet advertises stable per-Pod DNS through
its headless Service, including before readiness, so celld can reach retained
follower logs after Pod IPs change.

For PersistentFleet, capacity recommendations and manual requests enter the same
[current-operation executor](../current-operation/). The reservation records
intent before issuance and proof before effects. Bucket fleets have no current
operation: the workload controller rolls one member at a time and the operator
lowers replicas one member per completed rollout. Status is reconstructed from
current authority; clearing it cannot authorize deletion.

Read the [strict protocol](../../contracts/runtime-control-plane/),
[launcher contract](../../contracts/launcher-supervision/) and
[architecture decision](../../decisions/0022-celld-control-plane/) for details.
