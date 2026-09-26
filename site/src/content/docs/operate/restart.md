---
title: Restart a fleet
description: Roll a fleet one member at a time with a new restart token.
---

Request a restart with a new token:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"maintenance":{"restartToken":"restart-2026-09-20-1"}}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

The operator writes the token to the Pod template annotation
`celld.eric.dev/restart-token`, and members are replaced one at a time. Each
member drains on SIGTERM within `lifecycle.shutdownSeconds`. No downtime
permission is needed; `allowCoordinatedDowntime` is ignored. A Bucket fleet's
Deployment or StatefulSet performs the rolling update.

## PersistentFleet

The StatefulSet uses `OnDelete`, so the operator deletes each outdated Pod
itself: a member that is already down first, then running members from the
highest ordinal, each only once the fleet has [settled](../../concepts/current-operation/).
The replacement Pod reattaches the same disk. While waiting, the fleet reports
`LifecycleProgress` with `Rolling update waits before POD: REASON`.

A runtime without node-log state (before `0.5.1-ewhauser.5`) rolls on
readiness plus a one-minute stabilization after the last disruption.

The current token is not replayed; use a new token for another restart.
`maintenance.paused: true` suspends all workload changes (`MaintenancePaused`),
including lost-disk replacement. See [lifecycle troubleshooting](../../troubleshoot/lifecycle/)
if a rollout keeps waiting.
