---
title: Fleet profiles
description: Choose temporary Bucket disks or retained CSI disks for PersistentFleet.
---

Both profiles run the compatible celld fork directly and use a dedicated S3
bucket. celld owns durability and recovery and tolerates the loss of any one
member; the operator changes one member at a time. Bucket runs
`CELLD_DURABILITY=bucket`: every acknowledged write is in S3 first, so its disk
is a cache. PersistentFleet runs `CELLD_DURABILITY=fleet`: a write is
acknowledged once its followers' disks hold it.

| Profile/layout | Workload and local disk | Planned contraction |
| --- | --- | --- |
| Bucket / Deployment | Deployment with disk-backed `emptyDir`. | One member per step; Kubernetes chooses the deleted Pod. |
| Bucket / Ordered | StatefulSet with disk-backed `emptyDir`. | Highest ordinal, one per step. |
| PersistentFleet | StatefulSet with retained RWOP CSI disks. | Highest ordinal, one per step; its disk is kept and reattached if the fleet grows back. |

Use Ordered Bucket when you need deterministic zone assignment by ordinal.
Restart and upgrade roll one member at a time for both profiles.
PersistentFleet keeps each member's disk for the life of the fleet, including
across scale-in. Its StatefulSet waits for each restarted member to be Ready
before the next. It adds CSI requirements.

Profiles, Bucket layout, storage and placement are fixed at creation. There is no
conversion between them, and no adoption of fleets or disks from the former
operator or from another fleet. See
[storage configuration](../../configure/storage/) before choosing PersistentFleet.
