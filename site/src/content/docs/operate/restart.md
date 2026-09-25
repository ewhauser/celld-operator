---
title: Restart a fleet
description: Roll Bucket fleets one member at a time, or request coordinated PersistentFleet downtime.
---

Request a restart with a new token:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"maintenance":{"restartToken":"restart-2026-09-20-1"}}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

A Bucket fleet writes the token to the Pod template annotation
`celld.eric.dev/restart-token`, and its Deployment or StatefulSet replaces one
member at a time. Each member drains on SIGTERM within
`lifecycle.shutdownSeconds`. No downtime permission is needed.
`maintenance.paused: true` suspends workload changes (`MaintenancePaused`).

## PersistentFleet

A PersistentFleet restart stops the entire current fleet. Plan for interrupted
requests and connections, and add explicit downtime permission:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"maintenance":{"allowCoordinatedDowntime":true,"restartToken":"restart-2026-09-20-1"}}}'
```

The executor captures every current target and persists its strict runtime and
launcher proof before setting workload replicas to zero, then finishes old-disk
cleanup. Resume uses fresh disks and the recorded image;
completion waits for the intended Pods to become ready.

The current completed token is not replayed. Use a new token for another restart.
Do not send a new token to recover a blocked issued operation: it remains pending
until the original operation resolves.

`maintenance.paused: true` pauses new and unissued actions. An issued shutdown
cannot be cancelled by pausing, changing its token or extending a timeout.
See [blocked operations](../../troubleshoot/lifecycle/) for missing proof and
[the disk contract](../../contracts/disposable-disks/) for cleanup behavior.
