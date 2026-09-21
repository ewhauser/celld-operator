---
title: Scale a fleet
description: All replica requests pass through the strict current-operation executor.
---

Change the CelldFleet target, never its child Deployment or StatefulSet:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"replicas":4}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

Keep replicas at least equal to the configured zone count. Additions require
eligible node capacity; PersistentFleet also needs new CSI claims. New claims
are admitted by exact identity before scheduling gates open.

Scale-in is implemented for PersistentFleet and Ordered Bucket. The executor
selects one highest ordinal, records the operation, obtains strict celld and
launcher proof, then conditionally reduces replicas. PersistentFleet completes
only after exact disk cleanup. A request to remove several members proceeds one
operation at a time.

Bucket Deployment contraction is blocked because Kubernetes chooses the victim.
Select Ordered when creating a fleet that needs scale-in; layout is immutable.

Read `status.lifecycle` and conditions while the operation runs. A pause or new
replica target can cancel only before issuance. After that, the current request
must finish or remain visibly blocked before a new target can proceed.

An external autoscaler must target the CelldFleet `/scale` subresource using
`capacity.mode: External`. It uses the same safety checks and gains no direct
workload authority. See [capacity policy](../capacity/) and [current operations](../../concepts/current-operation/).
