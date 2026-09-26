---
title: Scale a fleet
description: Fleets scale in one member at a time; a PersistentFleet keeps a removed member's disk and reattaches it when the fleet grows back.
---

Change the CelldFleet target, never its child Deployment or StatefulSet:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"replicas":4}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

Keep replicas at least equal to the configured zone count. Additions require
eligible node capacity; PersistentFleet also needs new CSI claims for ordinals
that have not had a member before.

Bucket fleets raise replicas directly. Scale-in lowers replicas by one member
per step and waits for updated, ready replicas before the next. Ordered removes
the highest ordinal; a Deployment lets Kubernetes choose the victim, and the
member drains on SIGTERM. Automatic and External contraction also require
survivor capacity for every possible victim, else `CapacityUncertain`.

PersistentFleet scale-in follows the same rule: one member per step, the
highest ordinal, and only after the previous change has rolled out. The removed
member drains on SIGTERM and its claim, such as `data-my-fleet-3`, is kept.
Growth is applied in one step. An ordinal that had a member before reattaches
its kept claim, which celld treats as a restart on the same disk; new ordinals
get new claims. A claim at a member's name without this fleet's UID label stops
growth with `StorageIdentityConflict`. See the
[PersistentFleet lifecycle](../../concepts/current-operation/).

A removed member's disk costs storage until the fleet grows back to that
ordinal or is deleted. Delete its claim by hand only if the fleet will not grow
back. celld stops depending on a departed member once its leaders re-form their
ensembles, usually within seconds of the removal; a claim deleted before then
can still hold the only copy of recent writes.

Read conditions while scaling runs. An external autoscaler must target the
CelldFleet `/scale` subresource using `capacity.mode: External`. It uses the same
checks and gains no direct workload authority. See [capacity policy](../capacity/).
