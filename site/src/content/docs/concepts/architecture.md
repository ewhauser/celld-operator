---
title: Architecture
description: celld owns durability and recovery; the operator paces Kubernetes changes one member at a time.
---

A CelldFleet describes one independent celld cluster. The operator creates its
Kubernetes resources and coordinates lifecycle requests. Each fleet has a
dedicated bucket and a permanent storage reservation bound to its UID.

![The operator places celld Pods and coordinates their lifecycle. celld owns all S3 data access.](../../../assets/architecture.svg)

| Component | Responsibility |
| --- | --- |
| celld | Application execution, replication, tiering and recovery. It tolerates the loss of any one member and reports node-log state. |
| Operator | Placement, capacity observation, one-member rollouts and scale-in, and for PersistentFleet, settlement checks and disk release. |
| Kubernetes and CSI | Scheduling, resource versions, storage protection, attachment and volume deletion. |

Both profiles run celld directly; there is no launcher. Bucket uses a
Deployment or Ordered StatefulSet with temporary disk and
`CELLD_DURABILITY=bucket`. PersistentFleet uses a StatefulSet with retained CSI
disks and `CELLD_DURABILITY=fleet`. All runtime/recovery members need the
compatible fork.

The operator creates application and headless Services, NetworkPolicies, a
PodDisruptionBudget and storage reservations. It has no S3 client or EC2
termination path. You supply buckets, runtime IAM roles, nodes, CSI, ingress,
TLS and DNS. PersistentFleet advertises stable per-Pod DNS through its headless
Service, including before readiness, so celld can reach retained follower logs
after Pod IPs change.

Capacity recommendations and manual requests take the same path: one member per
step. For PersistentFleet, the operator disrupts a running member only when every
member's node-log report shows the fleet has [settled](../current-operation/).
The reservation keeps only bookkeeping; status is reconstructed and clearing it
authorizes nothing.

Read the [lifecycle contract](../../contracts/current-operation/) and
[runtime control plane](../../contracts/runtime-control-plane/) for details.
