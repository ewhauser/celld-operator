---
title: Fleet profiles
description: What Bucket and PersistentFleet mean for durability, workload shape and retirement.
sidebar:
  order: 2
---

A profile defines where celld keeps its durable state and what evidence the operator needs to remove a member. It is selected once when the fleet is created. For a practical setup comparison, use [choose a profile](../../configure/profiles/).

## Bucket

A Bucket member writes through S3 and keeps no retained local volume. The default workload is a Deployment. An optional Ordered layout uses a StatefulSet and deterministic ordinals; it still uses disk-backed ephemeral space. When contracting, the operator must verify surviving membership and positively observe expired leases for every generation outside it. It does not treat disappearance of a Pod as proof that the process died. A completed Bucket removal is a **logical membership** result; status records any retired sessions whose physical liveness is unknown.

Read [Bucket scale-in](../../contracts/bucket-scale-in/) and [Ordered Bucket](../../contracts/ordered-bucket/) for the precise completion rules. Deployment-to-Ordered migration is one-way and requires coordinated downtime.

## PersistentFleet

A PersistentFleet member has a retained EBS volume. The StatefulSet and launcher tie an admitted generation to the disk and provide an authenticated stop receipt. Retirement must prove the old writer stopped, its log sealed, survivors admitted the change, and the retained volume still has its expected identity. A missing Pod, a detached volume or a reused claim name is insufficient. The operator can use a qualified graceful attachment handoff within one zone; EC2 termination of an unreachable donor is optional and tightly scoped.

Read [PersistentFleet lifecycle](../../contracts/persistent-fleet-lifecycle/) and [storage setup](../../configure/storage/). Claims remain after scale-in and deletion.

## Shared limits

Both profiles require a dedicated bucket, explicit zones, a runtime writer identity and a separate operator evidence-reader identity. Both use the same [lifecycle journal](../lifecycle-journal/) and [safety rules](../safety-model/). Automatic contraction against production evidence remains release-gated; [limitations](../../reference/limitations/) names the current qualification boundary. Neither profile has completed live AWS qualification.
