---
title: Choose a profile
description: Choose where durable data lives and understand the operational cost before creating a fleet.
sidebar:
  order: 1
---

A fleet has one immutable profile. Choose it before creating the CelldFleet; changing profiles means building a new fleet with a different dedicated bucket. Both paths are **experimental**, and neither has completed AWS qualification. See [limitations](../../reference/limitations/) before using either path.

| Question | Bucket | PersistentFleet |
| --- | --- | --- |
| Where do acknowledged writes survive? | In S3, subject to the pinned runtime's ownership and recovery protocol | On replicated peer disks, with retained EBS volumes and S3 recovery evidence |
| Local disk | Ephemeral disk-backed space; no PVC to recover | One retained PVC per ordinal |
| Workload | Deployment by default; optional Ordered StatefulSet | StatefulSet and launcher |
| What makes removal safe? | Verified logical membership and expired lease evidence | Verified stop receipt, sealed log, disk identity and attachment evidence |
| Best fit | You want bucket-backed durability and simpler local storage management | You want to evaluate peer-disk acknowledgements for a latency-sensitive workload and can operate retained-volume recovery |
| Operational cost | Dedicated bucket, runtime writer IAM, separate evidence reader IAM | All Bucket dependencies, EBS CSI, retained StorageClass, launcher image and careful volume recovery |

Bucket writes wait on object storage; the persistent profile uses peer disks in its acknowledgement path. This is an architectural tradeoff, not a measured latency guarantee. Evaluate both with your application, and account for the persistent profile's more involved failure recovery.

The Bucket profile can use the default Deployment layout or set bucketWorkload: Ordered at creation. Ordered gives stable ordinals and deterministic highest-ordinal removal, but uses a StatefulSet and disk-backed ephemeral storage. A Deployment fleet can make a one-way, downtime-authorized migration to Ordered; this is an advanced operation described in [the migration reference](../../contracts/bucket-migration/).

PersistentFleet requires a StorageClass with EBS CSI provisioning, Retain reclaim policy and WaitForFirstConsumer binding. Its PVCs remain after scale-in or deletion. A replacement claim with the same name is not automatically trusted. Read [storage setup](../storage/) and [scaling requirements](../../operate/scaling/).

For either profile, set qualification: Experimental, provide a dedicated bucket and runtime ServiceAccount, and list the zones you can actually serve. Start with [AWS identities](../aws/), then [placement](../placement/). The field reference is [CelldFleet API](../../api/celldfleet/).
