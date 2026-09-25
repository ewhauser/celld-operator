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
| `RoutingReady` | Optional route status: current Gateway acceptance or an Ingress address. Independent of fleet readiness; not a DNS/TLS/connectivity test. |
| `InfrastructureReady` | Required Kubernetes objects match; Pods may still be Pending. |
| `Blocked` | A requested action cannot proceed; read its reason and message. |
| `Progressing` | Provisioning or a current operation is advancing. |
| `MaintenancePaused` | New and unissued work is paused; issued work remains recoverable. |
| `Deleting` | A fleet deletion request is waiting for verified cleanup. |
| `DiskCleanupPending` | Current DeleteClaims phase is waiting for exact CSI cleanup; not historical deletion authority. |
| `OperationSizeWarning` | Current authority is nearing its bounded encoded-state limit. |

`status.lifecycle` gives the operation ID, phase, fixed deadline, target and
blocker. `lastOutcome` is an informational last completion projection. Editing
status cannot alter current authority.

Configuration blockers include `NamespaceAccessDenied`, `ServiceAccountMissing`,
`StorageClassMissing`, `InvalidStorageClass`, `IsolationUnverified` and
`StorageScopeConflict`. Fix the named prerequisite; do not bypass identity checks.

`PodCompositionUnsupported` means an admitted Pod could not be maintained
safely, and the message names the Pod, container and field. The fleet can
still serve (`Ready` is unchanged), but a restart, upgrade or scale-in would
block. Adjust the admission policy that caused it; see
[admission mutations](../security-boundaries/#admission-mutations).

`InfrastructureBlocked` means a generated Service, NetworkPolicy or
PodDisruptionBudget differs from the operator's spec, has an owner, belongs to
another fleet or is being deleted. The operator does not adopt or repair it. The
one exception is a field a newer release declares that the live object leaves
unset, such as the peer port's `appProtocol`; the operator fills only that field,
pinned to the resourceVersion it verified. A value someone else set, including a
different `appProtocol`, still blocks until the generated spec is restored.

`DiskRetired` and `LauncherBlocked` replace `Provisioning` when an unready
replica's launcher reports that it will never start the runtime. The message
names the Pod. `DiskRetired` means the Pod's disk was retired after an earlier
Pod on it stopped without an operator request; see
[recovery](../../troubleshoot/recovery/).

Operational blockers identify unsupported layout contraction, incomplete runtime
or launcher proof, changed workload/storage identity, pending CSI cleanup and
expired operations. An expired deadline never proves a shutdown was unissued or
safe. Failed or ambiguous completion preserves disks and the current operation.

Capacity reasons such as `IncompleteMetrics`, `RepeatedSamples`, `PendingCapacity`,
`IneffectiveCapacity`, `StabilizingOut`, `StabilizingIn`, `ObservingRedistribution`
and `LoadNotRedistributed` describe demand observation rather than durability.
See [capacity](../../operate/capacity/) and [troubleshooting](../../troubleshoot/).
