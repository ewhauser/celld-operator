---
title: Scale a fleet
description: Fleets scale one member at a time; PersistentFleet waits for celld to settle and keeps a removed disk until no session needs it.
---

Change the CelldFleet target, never its child Deployment or StatefulSet:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"replicas":4}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

Keep replicas at least equal to the configured zone count. Additions require
eligible node capacity; PersistentFleet also needs new CSI claims.

Bucket fleets raise replicas directly. Scale-in lowers replicas by one member
per step and waits for updated, ready replicas before the next. Ordered removes
the highest ordinal; a Deployment lets Kubernetes choose the victim, and the
member drains on SIGTERM. Automatic and External contraction also require
survivor capacity for every possible victim, else `CapacityUncertain`.

PersistentFleet scale-in waits until the fleet is
[settled](../../concepts/current-operation/), then removes the highest ordinal.
Its claim is kept until a fresh celld sweep lists no session that needs it;
meanwhile status reads `Retaining disk data-my-fleet-3 of removed member my-fleet-3:
still needed by ...`. The next step waits for that disk and for the fleet to
settle again. Growth waits until retained disks of removed members are deleted
and never reuses them. Scale-in needs runtime `0.5.1-ewhauser.6` or later; `.5` reports node-log state, but an idle `.5` leader never lets go of a removed follower, so its disk is never released.

Read conditions while scaling runs. An external autoscaler must target the
CelldFleet `/scale` subresource using `capacity.mode: External`. It uses the same
checks and gains no direct workload authority. See [capacity policy](../capacity/).
