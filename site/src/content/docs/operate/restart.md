---
title: Restart a fleet
description: Request a journaled same-version restart and watch each replacement settle.
---

A planned restart uses a new `spec.maintenance.restartToken`. It restarts the pinned runtime one exact Pod at a time, unless a small PersistentFleet explicitly uses coordinated downtime. It does not change the runtime image. The operator may refuse the request when placement, survivor, fencing, or recovery evidence is insufficient.

## Check readiness and request a restart

Use an explicit context, namespace, and a token that has never been used on this fleet. Check the current condition and operation first:

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status.lifecycle'
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet \
  --type merge -p '{"spec":{"maintenance":{"restartToken":"restart-2026-09-19-01"}}}'
```

If `maintenance` already contains `paused` or `allowCoordinatedDowntime`, preserve those values when editing it. Unpause before expecting a new action. For a one- or two-member PersistentFleet, a same-version restart requires `allowCoordinatedDowntime: true`; this deliberately stops the whole fleet. A one-member target also requires one configured AZ. To approve that outage and request the restart together:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet \
  --type merge -p '{"spec":{"maintenance":{"allowCoordinatedDowntime":true,"restartToken":"restart-with-downtime-01"}}}'
```

Use a new token for this request. Keep application traffic stopped until the completion checks below pass. The permission persists in the spec; after the operation completes, set `allowCoordinatedDowntime` back to `false` if later operations must not stop the whole fleet.

## Watch progress

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -w
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
```

`status.lifecycle` shows the operation, phase, target, and last outcome. The controller captures Pod UIDs, authorizes each deletion durably, checks recovery, and waits for a stable interval before the next target. A completed token cannot replay; use a new token for a later restart. Changing the token or pausing stops only actions not yet issued. An issued action must finish recovery.

Completion means the restart operation has cleared, the last outcome records completion, all requested replicas are ready, and no blocker remains. If `DisruptionUnqualified`, `CoordinatedDowntimeBlocked`, `RecoveryBlocked`, or `MaintenanceRecoveryBlocked` appears, follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/). Do not manually delete the target Pod or clear its journal entry.
