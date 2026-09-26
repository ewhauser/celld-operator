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
permission is needed; `allowCoordinatedDowntime` is ignored. The workload
controller performs the rolling update: a Bucket fleet's Deployment or
StatefulSet, or a PersistentFleet's StatefulSet.

## PersistentFleet

The StatefulSet uses `RollingUpdate`. Kubernetes restarts one member at a time,
highest ordinal first, and waits for each to be Ready before the next. Readiness
is celld's health endpoint, which returns 200 only after the member has
recovered its previous session. The replacement Pod reattaches the same disk.
The operator deletes no Pods to restart them. While the rollout runs, the fleet
reports `Provisioning` with `Rolling out one member at a time; waiting for updated, ready replicas`.
See the [PersistentFleet lifecycle](../../concepts/current-operation/).

The current token is not replayed; use a new token for another restart.
`maintenance.paused: true` stops the operator from writing workload changes
(`MaintenancePaused`); a rollout the workload controller has already started
continues. See [lifecycle troubleshooting](../../troubleshoot/lifecycle/)
if a rollout keeps waiting.
