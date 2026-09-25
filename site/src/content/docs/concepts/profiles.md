---
title: Fleet profiles
description: Choose temporary Bucket disks or disposable CSI disks for PersistentFleet.
---

Both profiles run the compatible celld fork and use a dedicated S3 bucket. celld
owns durability and recovery. Bucket runs `CELLD_DURABILITY=bucket`: every
acknowledged write is in S3 first, so its disk is a cache and losing any member
loses no acknowledged write. Bucket Pods run celld directly and change one member
at a time. PersistentFleet runs under a launcher, and the operator consumes its
strict shutdown result before removing a member.

| Profile/layout | Workload and local disk | Planned contraction |
| --- | --- | --- |
| Bucket / Deployment | Deployment with disk-backed `emptyDir`. | One member per step; Kubernetes chooses the deleted Pod. |
| Bucket / Ordered | StatefulSet with disk-backed `emptyDir`. | Highest ordinal, one per step. |
| PersistentFleet | StatefulSet with dynamically provisioned RWOP CSI disks. | Exact highest ordinal; strict shutdown and CSI cleanup before completion. |

Use Ordered Bucket when you need deterministic zone assignment by ordinal.
Bucket restart and upgrade roll one member at a time without downtime.
PersistentFleet adds local peer storage and CSI requirements. Its disks are
**disposable after verified strict shutdown**; they are not a retained-disk
recovery archive. Growth and coordinated restart use fresh claims.
PersistentFleet restart and upgrade stop the whole fleet.

Profiles, Bucket layout, storage and placement are fixed at creation. There is no
migration or adoption path for old fleets or previously retained disks. See
[storage configuration](../../configure/storage/) before choosing PersistentFleet.
