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
| PersistentFleet | StatefulSet with retained RWOP CSI disks. | Highest ordinal when settled; its disk is deleted once no session needs it. |

Use Ordered Bucket when you need deterministic zone assignment by ordinal.
Restart and upgrade roll one member at a time for both profiles.
PersistentFleet keeps each member's disk across restart and upgrade and waits
for celld to report the fleet settled before disrupting the next member. It adds
CSI requirements and needs runtime `0.5.1-ewhauser.6` or later for scale-in.

Profiles, Bucket layout, storage and placement are fixed at creation. There is no
migration or adoption path for old fleets or previously retained disks. See
[storage configuration](../../configure/storage/) before choosing PersistentFleet.
