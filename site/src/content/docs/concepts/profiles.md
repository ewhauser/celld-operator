---
title: Fleet profiles
description: Choose temporary Bucket disks or disposable CSI disks for PersistentFleet.
---

Both profiles run the compatible celld fork under a launcher and use a dedicated
S3 bucket. celld owns durability and recovery; the operator consumes its strict
shutdown result before removing a member.

| Profile/layout | Workload and local disk | Planned contraction |
| --- | --- | --- |
| Bucket / Deployment | Deployment with disk-backed `emptyDir`. | Blocked: Kubernetes chooses the deleted Pod. |
| Bucket / Ordered | StatefulSet with disk-backed `emptyDir`. | Exact highest ordinal after strict shutdown. |
| PersistentFleet | StatefulSet with dynamically provisioned RWOP CSI disks. | Exact highest ordinal; strict shutdown and CSI cleanup before completion. |

Use Ordered Bucket when you need scale-in without persistent local disks.
PersistentFleet adds local peer storage and CSI requirements. Its disks are
**disposable after verified strict shutdown**; they are not a retained-disk
recovery archive. Growth and coordinated restart use fresh claims.

Profiles, Bucket layout, storage and placement are fixed at creation. There is no
migration or adoption path for old fleets or previously retained disks. See
[storage configuration](../../configure/storage/) before choosing PersistentFleet.
