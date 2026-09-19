---
title: Conditions and blockers
description: The condition types a CelldFleet reports, the reasons that accompany Blocked, and what each one asks you to do.
---

Conditions live in `status.conditions`. `Blocked`, `LifecycleBlocked` and `ProductionQualified` are always present; `LifecycleBlocked=True` and `ProductionQualified=False` stay set throughout the experimental release. Events are emitted only when a blocker's status or reason changes.

## Condition types

| Type | True means | Notes |
| --- | --- | --- |
| `Ready` | The current workload generation reports the requested number of runtime-ready replicas. | Describes observed availability only. A paused or blocked fleet can be Ready. Not a recovery certificate. |
| `InfrastructureReady` | The required Kubernetes objects match the expected template. | Can be true while Pods are Pending. |
| `Blocked` | Some requested action cannot proceed. | The reason (below) says which. Compatible additive capacity may continue. |
| `LifecycleBlocked` | Always true in this release. | Contraction, upgrade and deletion paths carry explicit gates. |
| `LifecycleProgress` | A durable transition is in progress. | The reservation journal is authoritative; clearing status cancels nothing. |
| `MaintenancePaused` | `spec.maintenance.paused` has taken effect. | Pause is asynchronous; issued effects continue recovery. |
| `Deleting` | The fleet has a deletion timestamp and the finalizer is holding it. | Workload, PVCs and reservation are retained. |
| `ProductionQualified` | Never true in this release. | See [implementation status](../../contracts/critical-features/). |

## Blocker reasons

Reasons appear on `Blocked` and, for lifecycle operations, in `status.lifecycle`. Grouped by what to do about them.

### Fix the configuration or environment

| Reason | Meaning |
| --- | --- |
| `NamespaceAccessDenied` | The fleet namespace lacks the Role from `config/rbac/fleet-namespace.yaml`. Nothing in the namespace is created until it exists. |
| `InvalidConfiguration` | A cross-field combination admission could not catch; the message names it. |
| `ServiceAccountMissing` | `spec.serviceAccountName` does not exist in the namespace. |
| `StorageClassMissing`, `InvalidStorageClass` | The PersistentFleet StorageClass is absent or does not use `ebs.csi.aws.com` with `Retain` and `WaitForFirstConsumer`. |
| `IsolationUnverified` | The operator was started without `--network-policy-enforced`. Verify the CNI, then attest. |
| `StorageScopeConflict` | The bucket is already reserved by another fleet identity. Reservations are never freed; choose another bucket. |
| `InfrastructureBlocked` | An owned object drifted from the expected template or could not be created. The operator does not roll out disruptive repairs. |
| `BucketSchedulingBlocked`, `PersistentSchedulingBlocked` | Strict placement cannot be satisfied with current nodes. Check Pod events. |

### Wait, or supply what is missing

| Reason | Meaning |
| --- | --- |
| `Provisioning`, `Provisioned` | Progress states during initial creation. |
| `LifecycleProgress` | An operation is executing. `status.lifecycle` shows the phase. |
| `ScaleOutBlocked` | PVC allocation is uncertain or the replica update hit a conflict. |
| `ReplicaUpdateBlocked` | The workload compare-and-swap failed; the operator retries with fresh state. |
| `DesiredChanged`, `CapacityChanged` | Intent changed while an operation was unissued; the operation is being cancelled or re-planned durably. |
| `CapacityUncertain`, `EvidenceUnavailable` | Observation was incomplete or the evidence transport did not answer. Decisions wait for fresh, complete data. |
| `OperationStalled` | The persisted deadline passed. Issued effects still recover; inspect the target. |
| `MaintenancePaused` | New actions are suspended by `spec.maintenance.paused`. |
| `CoordinatedDowntimeBlocked` | The request needs `allowCoordinatedDowntime: true` or its preconditions are not met. |

### A gate that no configuration opens

| Reason | Meaning |
| --- | --- |
| `BucketCompletionUnqualified` | No evidence transport is configured (no production reader, no `--local-evidence`). |
| `FencingUnqualified` | The PersistentFleet was provisioned without the launcher and cannot produce stop receipts. |
| `BucketAutomaticUnqualified`, `PersistentAutomaticUnqualified` | Automatic contraction against production evidence waits for EKS and S3 release qualification. |
| `FollowerRetirementUnqualified` | A live PersistentFleet contraction would leave fewer than two nodes. Use coordinated downtime for 2-to-1. |
| `DisruptionUnqualified` | The requested restart or maintenance path has no qualified disruption contract. |
| `UnsupportedTransition` | The requested `runtimeImage` has no qualified adapter pair from the current pin, including any rollback. The request is retained and never edits a Pod. |
| `SessionBindingUnqualified`, `HistoricalSessionUnresolved` | Observed S3 sessions cannot be bound to admitted generations. Unknown historical writers are refused. |
| `LauncherIdentityBlocked`, `ReactivationBlocked` | The launcher's identity, lock or restart-denial state does not match what was journaled. |
| `MigrationBlocked` | Deployment-to-Ordered migration preconditions failed or evidence for a captured writer is missing. |

### Investigate before doing anything

| Reason | Meaning |
| --- | --- |
| `PossibleDataLoss` | A sticky loss finding read from the runtime's declarations. Requires investigation and recovery. Never clear the journal. |
| `StorageIdentityConflict` | A retained claim is missing, replaced, adopted or has uncertain creation history. Retained disks require manual review. |
| `RecoveryBlocked`, `MaintenanceRecoveryBlocked` | An issued effect could not be confirmed complete. Uncertain completion is a blocker, not a retry. |
| `DeletionBlocked` | RetainData deletion is waiting for exact stop receipts or sealed logs. Do not remove the finalizer. |
| `JournalInvalid` | The reservation journal or an archive page failed validation. Restore from backup; do not edit. |

## Capacity policy reasons

`status.capacity.reason` explains a policy decision rather than a blocker: `PendingCapacity` (requested replicas not yet useful), `IneffectiveCapacity` (past the provisioning deadline), `IncompleteMetrics`, `RepeatedSamples`, `RateLimited`, `StabilizingOut`, `StabilizingIn`, `ObservingRedistribution`, `LoadNotRedistributed`, `RedistributionUnknown` and `ManualOverride`. See [capacity policy](../../contracts/capacity-policy/).
