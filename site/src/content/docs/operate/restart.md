---
title: Restart a fleet
description: Request coordinated downtime and strict shutdown of the current working set.
---

A restart stops the entire current fleet. Plan for interrupted requests and
connections, then request a new token with explicit downtime permission:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"maintenance":{"allowCoordinatedDowntime":true,"restartToken":"restart-2026-09-20-1"}}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

The executor captures every current target and persists its strict runtime and
launcher proof before setting workload replicas to zero. PersistentFleet then
finishes old-disk cleanup. Resume uses fresh disks and the recorded image;
completion waits for the intended Pods to become ready.

The current completed token is not replayed. Use a new token for another restart.
Do not send a new token to recover a blocked issued operation: it remains pending
until the original operation resolves.

`maintenance.paused: true` pauses new and unissued actions. An issued shutdown
cannot be cancelled by pausing, changing its token or extending a timeout.
See [blocked operations](../../troubleshoot/lifecycle/) for missing proof and
[the disk contract](../../contracts/disposable-disks/) for cleanup behavior.
