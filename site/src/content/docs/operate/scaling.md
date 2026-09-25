---
title: Scale a fleet
description: Bucket fleets scale one member at a time; PersistentFleet requests pass through the strict current-operation executor.
---

Change the CelldFleet target, never its child Deployment or StatefulSet:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"replicas":4}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

Keep replicas at least equal to the configured zone count. Additions require
eligible node capacity; PersistentFleet also needs new CSI claims. New claims
are admitted by exact identity before scheduling gates open.

Bucket fleets raise replicas directly. Scale-in lowers replicas by one member
per step and waits for updated, ready replicas before the next. Ordered removes
the highest ordinal; a Deployment lets Kubernetes choose the victim, and the
member drains on SIGTERM. Automatic and External contraction also require
survivor capacity for every possible victim, else `CapacityUncertain`.

PersistentFleet scale-in selects one highest ordinal, records the operation,
obtains strict celld and launcher proof, then conditionally reduces replicas. It
completes only after exact disk cleanup. A request to remove several members
proceeds one operation at a time.

Read `status.lifecycle` and conditions while scaling runs. For PersistentFleet,
a pause or new replica target can cancel only before issuance. After that, the
current request must finish or remain visibly blocked before a new target can
proceed.

An external autoscaler must target the CelldFleet `/scale` subresource using
`capacity.mode: External`. It uses the same safety checks and gains no direct
workload authority. See [capacity policy](../capacity/) and [current operations](../../concepts/current-operation/).
