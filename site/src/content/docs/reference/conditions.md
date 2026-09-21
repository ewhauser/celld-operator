---
title: Conditions and blockers
description: Use conditions and current-operation status to locate the blocked boundary.
---

Inspect conditions and the current operation together:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet   -o json | jq '.status | {conditions, lifecycle, capacity}'
```

| Signal | Meaning |
| --- | --- |
| `Ready` | The observed workload generation has ready replicas. Availability is not deletion proof. |
| `InfrastructureReady` | Required Kubernetes objects match; Pods may still be Pending. |
| `Blocked` | A requested action cannot proceed; read its reason and message. |
| `Progressing` | Provisioning or a current operation is advancing. |
| `MaintenancePaused` | New and unissued work is paused; issued work remains recoverable. |
| `Deleting` | A fleet deletion request is waiting for verified cleanup. |
| `DiskCleanupPending` | Current DeleteClaims phase is waiting for exact CSI cleanup; not historical deletion authority. |
| `OperationSizeWarning` | Current authority is nearing its bounded encoded-state limit. |
| `ProductionQualified` | False while cloud qualification remains outstanding. |
| `LifecycleBlocked` | Always true with `QualificationIncomplete` in this experimental implementation, even after successful operations. Use `Blocked` and `status.lifecycle` for an actual stalled request. |

`status.lifecycle` gives the operation ID, phase, fixed deadline, target and
blocker. `lastOutcome` is an informational last completion projection. Editing
status cannot alter current authority.

Configuration blockers include `NamespaceAccessDenied`, `ServiceAccountMissing`,
`StorageClassMissing`, `InvalidStorageClass`, `IsolationUnverified` and
`StorageScopeConflict`. Fix the named prerequisite; do not bypass identity checks.

Operational blockers identify unsupported layout contraction, incomplete runtime
or launcher proof, changed workload/storage identity, pending CSI cleanup and
expired operations. An expired deadline never proves a shutdown was unissued or
safe. Failed or ambiguous completion preserves disks and the current operation.

Capacity reasons such as `IncompleteMetrics`, `RepeatedSamples`, `PendingCapacity`,
`IneffectiveCapacity`, `StabilizingOut`, `StabilizingIn`, `ObservingRedistribution`
and `LoadNotRedistributed` describe demand observation rather than durability.
See [capacity](../../operate/capacity/) and [troubleshooting](../../troubleshoot/).
