---
title: Conditions and blockers
description: Use conditions, reasons and messages to locate what a fleet waits for.
---

Inspect conditions and lifecycle status together:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet   -o json | jq '.status | {conditions, lifecycle, capacity}'
```

| Signal | Meaning |
| --- | --- |
| `Ready` | The observed workload generation has ready replicas. Availability is not settlement. |
| `RoutingReady` | Optional route status: current Gateway acceptance or an Ingress address. Independent of fleet readiness; not a DNS/TLS/connectivity test. |
| `InfrastructureReady` | Required Kubernetes objects match; Pods may still be Pending. |
| `Blocked` | A requested action cannot proceed; read its reason and message. |
| `Progressing` | `Provisioning` or `LifecycleProgress`: a change is rolling out or waiting for the fleet to settle. |
| `MaintenancePaused` | Workload changes are suspended. |
| `Deleting` | A fleet deletion request is removing compute and, for PersistentFleet, disks. |
| `OperationSizeWarning` | Reservation bookkeeping is nearing its bounded encoded-state limit. |

Earlier releases also set `DiskCleanupPending`; it is removed as fleets
reconcile. `status.lifecycle` no longer shows operation phases; `lastOutcome`
may read `Superseded` for an operation dropped on upgrade from an earlier
release. Editing status authorizes nothing.

Configuration blockers include `NamespaceAccessDenied`, `ServiceAccountMissing`,
`StorageClassMissing`, `InvalidStorageClass`, `IsolationUnverified` and
`StorageScopeConflict`. Fix the named prerequisite; do not bypass identity checks.

Both profiles converge drift in the workload template and replicas,
NetworkPolicy and PodDisruptionBudget back to the operator's spec. Services stay
verify-only; the one in-place change fills a field a newer release declares that
the live object leaves unset, such as the peer port's `appProtocol`. Objects
without this fleet's UID label, or with owner references, are refused:
`LifecycleBlocked` for the workload, `InfrastructureBlocked` for prerequisites.

| Reason | Meaning |
| --- | --- |
| `Provisioning` | Workload created or members rolling out; waiting for updated, ready replicas. |
| `Provisioned` | All members ready; for PersistentFleet, the fleet is also settled. |
| `LifecycleProgress` | A change is proceeding or waiting: a rolling update, contraction or replacement waiting to settle, a retained disk still needed by named sessions, a lost-disk replacement, or deletion. |
| `InfrastructureBlocked` | A prerequisite object could not be created or converged. |
| `LifecycleBlocked` | The workload or its Pods are not owned by this fleet. |
| `StorageIdentityConflict` | A PVC at a member's name does not carry this fleet's UID label; it is never adopted. |
| `ReplaceMemberInvalid` | `celld.eric.dev/replace-member` names no current member. |
| `CapacityUncertain` | Automatic or External contraction lacks survivor-capacity evidence. |
| `SchedulingBlocked` | A scheduling gate could not be released. |
| `MaintenancePaused` | `maintenance.paused` suspends workload changes. |
| `UnsupportedTransition` | The runtime is not a digest-pinned fork pin. |
| `DeletionBlocked` | The StatefulSet to delete is not owned by this fleet. |

PersistentFleet also emits Warning Events `MemberDiskLost` and `MemberReplaced`
when it replaces a member's disk; the message names sessions celld may record as
lost. Earlier releases reported `DiskRetired`, `LauncherBlocked` and
`PodCompositionUnsupported`; current releases do not.

Capacity reasons such as `IncompleteMetrics`, `RepeatedSamples`, `PendingCapacity`,
`IneffectiveCapacity`, `StabilizingOut`, `StabilizingIn`, `ObservingRedistribution`
and `LoadNotRedistributed` describe demand observation rather than durability.
See [capacity](../../operate/capacity/) and [troubleshooting](../../troubleshoot/).
