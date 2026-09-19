---
title: Fleet profiles
description: Bucket and PersistentFleet trade acknowledgement latency for different durability bases, and each has its own contraction contract.
sidebar:
  order: 2
---

A `CelldFleet` declares one of two profiles. The choice is immutable and selects both the Kubernetes workload shape and the evidence the operator needs before it will remove a member. [ADR 0002](../../decisions/0002-durability-and-scaling/) put both in scope; the contracts below are what executes today.

## Side by side

| | Bucket | PersistentFleet |
| --- | --- | --- |
| Durability basis | S3 durability plus an ownership check before acknowledgement | Peer disks; retained EBS volumes carry recovery authority |
| Workload | Deployment by default; StatefulSet with disk-backed `emptyDir` when `bucketWorkload: Ordered` | StatefulSet with retained, `ReadWriteOncePod` PVCs |
| Runtime identity | Pod UID | Launcher-seeded lease generation bound to the disk |
| Storage fields | `bucket`, `region`, `sizeGiB` (disk limit) | plus `storageClassName` (retained EBS class) |
| Scale-out | Journaled, additive, both profiles | same |
| Manual contraction | Executes under logical membership completion | Executes through the launcher's graceful retirement path |
| Automatic contraction | Local fixture only; `BucketAutomaticUnqualified` in production | Local fixture only; `PersistentAutomaticUnqualified` in production |
| Contraction floor | Down to `azCount` | Two live nodes without downtime; 2-to-1 only with coordinated downtime |
| Restart | Rolling, one exact Pod UID at a time | Rolling with at least two survivors |
| Runtime upgrade | Not implemented | v0.4.1 to v0.5.0 with whole-fleet downtime |
| Layout migration | One-way Deployment to Ordered with downtime | not applicable |
| EC2 fencing | no | opt-in, for one admitted unreachable donor |

## Bucket

Bucket fleets keep no retained local state. Every member writes through S3, so the operator's completion contract for a removal is about **logical membership**, not physical process death: the requested decrement has converged in Kubernetes, every survivor is fresh and healthy, and every generation outside current membership has a positively observed expired lease with no peer-log recovery obligation, held through a ten-second settling window. The number of retired generations whose physical liveness is unknown is reported in `status.lifecycle.retiredBucketSessions`.

Two layouts exist. The default Deployment layout lets Kubernetes pick the victim, so the operator journals every candidate before one decrement. The `Ordered` layout uses a StatefulSet with an ordinal-to-zone assignment released through scheduling gates, giving deterministic highest-ordinal retirement and a retained ordinal prefix that always covers the listed zones. Existing Deployment fleets can migrate to Ordered once, with downtime.

Read: [Bucket scale-in](../../contracts/bucket-scale-in/), [Ordered Bucket fleets](../../contracts/ordered-bucket/), [Bucket migration](../../contracts/bucket-migration/), [ADR 0016](../../decisions/0016-bucket-preflight-and-completion-boundary/).

## PersistentFleet

PersistentFleet members own a retained EBS volume, so a removal must positively stop the writer before the disk can be reused. The operator injects a launcher beside the unmodified celld binary. The launcher holds an exclusive lock on the volume, seeds the runtime's lease generation so the operator knows it independently of HTTP, and answers HMAC-authenticated stop requests on a private port. A `Stopped` receipt is produced only after the child has exited and the launcher has reacquired the same lock file; it then writes a durable marker that denies any restart of the retired Pod UID.

Contraction stops the highest ordinal, waits for its own log to seal and expire, admits its retirement in the survivors' view, and only then decrements the StatefulSet. Graceful cross-node PVC handoff within one zone is implemented under an explicit CSI attachment contract. Uncertain node failure remains blocked unless EC2 fencing is enabled for an already-admitted donor.

Initial provisioning is deliberately strict: the operator creates every ordinal PVC exclusively before the StatefulSet can consume it, rejects pre-existing claims even with matching labels, and blocks for review if a creation attempt's outcome is uncertain. Legacy fleets provisioned without the launcher stay on the old path and report `FencingUnqualified` for contraction.

Read: [PersistentFleet lifecycle](../../contracts/persistent-fleet-lifecycle/), [EC2 fencing](../../contracts/infrastructure-fencing/), [ADR 0017](../../decisions/0017-persistent-launcher-and-graceful-retirement/), [runtime dependencies](../../contracts/runtime-dependencies/) for the conversions that are not implemented.

## Placement, in both profiles

`placement.zones` is an explicit allowlist of standard zone names in the storage region, with `azCount` equal to its length and `replicas` at least that count. `Strict` (the default) spreads with `maxSkew: 1` and `DoNotSchedule`, and requires distinct hostnames; missing capacity leaves Pods Pending. `Relaxed` keeps the allowlist but turns spread and hostname separation into preferences. Neither mode reselects zones or moves retained volumes, and Kubernetes placement does not control which celld followers a cell uses. See [ADR 0004](../../decisions/0004-availability-zone-placement/).
