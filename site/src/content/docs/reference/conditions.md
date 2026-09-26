---
title: Conditions and blockers
description: Use conditions, reasons and messages to locate what a fleet waits for.
---

Inspect conditions and capacity status together:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet   -o json | jq '.status | {conditions, capacity}'
```

| Signal | Meaning |
| --- | --- |
| `Ready` | Every replica of the observed workload generation is ready. Availability does not mean celld has finished recovering every session. |
| `RoutingReady` | Optional route status: current Gateway acceptance or an Ingress address. Independent of fleet readiness; not a DNS/TLS/connectivity test. |
| `InfrastructureReady` | Required Kubernetes objects match; Pods may still be Pending. |
| `Blocked` | A requested action cannot proceed; read its reason and message. |
| `Progressing` | `Provisioning` while a change rolls out or a down member waits to be replaced, or `LifecycleProgress` while the operator heals a member or deletes the fleet. |
| `MaintenancePaused` | Workload changes are suspended. |
| `Deleting` | A fleet deletion request is removing compute and, for PersistentFleet, disks. |
| `OperationSizeWarning` | The reservation's capacity-policy history is nearing its bounded encoded-state limit. |

Earlier releases also set `DiskCleanupPending`; it is removed as fleets
reconcile. `status.lifecycle` is always empty. Editing status authorizes
nothing.

Configuration blockers include `InvalidConfiguration`, `NamespaceAccessDenied`,
`ServiceAccountMissing`, `StorageClassMissing`, `InvalidStorageClass`,
`IsolationUnverified` and `StorageScopeConflict`. Fix the named prerequisite; do
not bypass identity checks. Seeded fleets can also report
`SeedReservationBlocked`, `SeedInitializing` and `SeedCancellationBlocked`; see
[preview seeding](../../contracts/preview-seeding/).

Both profiles converge drift in the workload's template, replicas and update
strategy, and in the NetworkPolicy and PodDisruptionBudget, back to the
operator's spec. Services stay verify-only; the one in-place change fills a
field a newer release declares that the live object leaves unset, such as the
peer port's `appProtocol`. Objects without this fleet's UID label, or with owner
references, are refused: `LifecycleBlocked` for the workload,
`InfrastructureBlocked` for prerequisites.

| Reason | Meaning |
| --- | --- |
| `Provisioning` | Workload created, members rolling out, or a scale-in step waiting for the previous change to roll out. For PersistentFleet, also one member down and waiting for the replacement delay; the message gives the time it will be replaced on a fresh disk unless it returns. |
| `Provisioned` | The workload has rolled out and all replicas are ready. |
| `LifecycleProgress` | The operator took a self-healing action, named in the message: it force-deleted a Pod left on a node that no longer answers, or it is replacing a member on a fresh disk. Also reported while fleet deletion removes the workload and then, for PersistentFleet, every fleet PVC. |
| `InfrastructureBlocked` | A prerequisite object or the workload could not be created or converged. |
| `LifecycleBlocked` | The workload is not owned by this fleet. |
| `StorageIdentityConflict` | A PVC at a member's name does not carry this fleet's UID label; it is never adopted. Creation and growth wait. |
| `CapacityUncertain` | Automatic or External contraction lacks survivor-capacity evidence. |
| `SchedulingBlocked` | An Ordered Bucket zone scheduling gate could not be released. |
| `MaintenancePaused` | `maintenance.paused` suspends workload changes. |
| `UnsupportedTransition` | The runtime is not a digest-pinned fork pin. |
| `DeletionBlocked` | The workload to delete is not owned by this fleet. |

The operator emits these Events on the fleet:

| Event | Type | Meaning |
| --- | --- | --- |
| `MemberForceDeleted` | Warning | A member Pod was still present more than two minutes after its termination grace ended, so its node no longer answers. The operator force-deleted it; the member returns on its own disk. PersistentFleet and Ordered Bucket. |
| `MemberDiskLost` | Warning | A member's claim was `Lost`: its volume no longer exists. The operator deleted the claim and the Pod; the member returns on a fresh disk. |
| `MemberReplaced` | Warning | One member stayed down for the replacement delay while every other member had been ready for five minutes. The operator deleted its claim and Pod; the member returns on a fresh disk. |
| `OperationSuperseded` | Normal | The operator dropped an operation that an earlier release left in flight. |

Other Events record changes to the `Blocked` condition's status or reason.
Earlier releases reported `DiskRetired`, `LauncherBlocked` and
`PodCompositionUnsupported`; current releases do not. See
[self-healing](../../concepts/current-operation/#self-healing).

Capacity reasons such as `IncompleteMetrics`, `RepeatedSamples`, `PendingCapacity`,
`IneffectiveCapacity`, `StabilizingOut`, `StabilizingIn`, `ObservingRedistribution`
and `LoadNotRedistributed` describe demand observation rather than durability.
See [capacity](../../operate/capacity/) and [troubleshooting](../../troubleshoot/).
