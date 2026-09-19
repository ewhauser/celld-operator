---
title: Lifecycle operations stuck
description: Diagnose blocked scale-in, restart, upgrade, and RetainData deletion without losing recovery evidence.
---

Read the current request, operation phase, and condition reason before changing anything. The reservation journal is authoritative, including after a controller restart or cleared status.

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status.lifecycle'
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
kubectl --context YOUR_CONTEXT -n fleets get events --sort-by=.lastTimestamp
```

| Reason or symptom | Next check |
| --- | --- |
| `MaintenancePaused` | Check `spec.maintenance.paused`. New actions wait; already issued recovery still runs. Resume only when you are ready for the next action. |
| `CoordinatedDowntimeBlocked` | Confirm whether this small PersistentFleet operation needs `allowCoordinatedDowntime: true`; read the outage implications in [restart](../../operate/restart/) or [runtime upgrade](../../operate/upgrade-runtime/). |
| `BucketCompletionUnqualified`, `FencingUnqualified` | The needed completion transport or trusted launcher is absent. Do not substitute Pod termination or a force detach for evidence. |
| `BucketAutomaticUnqualified`, `PersistentAutomaticUnqualified` | Production automatic contraction is behind a release qualification gate. Use the supported manual procedure only if its own checks pass. |
| `UnsupportedTransition` | Compare current and requested image with [compatibility](../../reference/compatibility/). Only v0.4.1 → v0.5.0 on launcher-managed PersistentFleet is implemented. |
| `OperationStalled`, `RecoveryBlocked`, `MaintenanceRecoveryBlocked` | Inspect the named target, Pod events, PVC, launcher, and operator logs. The operation may have issued an effect and must recover that exact effect. |
| `DeletionBlocked` | Identify the missing exact stop receipt, sealed log, lease expiry, or historical writer evidence. The finalizer retains the fleet until proof is complete. |
| `PossibleDataLoss`, `JournalInvalid`, `StorageIdentityConflict` | Preserve disks, bucket contents, reservation, and journal archives. Investigate or restore evidence; do not clear the journal. |

Check the manager logs if the condition does not explain the wait:

```bash
kubectl --context YOUR_CONTEXT -n celld-system logs deployment/celld-celld-operator --tail=200
```

An operation can be blocked even while `Ready=True`: that condition describes the currently running workload size. `Progressing=True` indicates provisioning or an active lifecycle action. A deletion request may stay visible under its finalizer until all captured writers are accounted for. [Conditions](../../reference/conditions/) lists the reasons; [limitations](../../reference/limitations/) marks gates that still need qualification.

Completion means the requested lifecycle outcome is recorded, the current operation is clear, and the fleet is ready at the resulting count. For deletion, completion means the `CelldFleet` object disappears while the documented recovery assets remain. Never remove its finalizer, edit reservation annotations, or force delete a Pod as a routine repair.
