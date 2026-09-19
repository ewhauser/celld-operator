---
title: Lifecycle journal
description: The storage reservation is the single mutable pointer to lifecycle authority; every operation is persisted before it is issued and recovered after any crash.
sidebar:
  order: 3
---

Every destructive step the operator takes is written down first. The record lives on the fleet's `CelldStorageReservation`, a cluster-scoped object created when the bucket is bound to the fleet UID. [ADR 0012](../../decisions/0012-restart-safe-manual-lifecycle/) made this the foundation for all scaling; [ADR 0014](../../decisions/0014-coordinated-maintenance/) and [ADR 0015](../../decisions/0015-shared-lifecycle-safety/) extended it to maintenance, deadlines and cancellation authority.

## What the journal holds

- The current operation: kind, ID, phase, from and to counts, selected target, issued effects and their receipts, deadline, and stall state.
- Every generation ever admitted to the fleet, with the session evidence observed for it. History is never pruned.
- Positive stop receipts, restart-denial bits, loss fences and recovery epochs.
- Claim identities: the exact UID of every PVC the operator created, so a replaced disk is detected as `StorageIdentityConflict`.
- Capacity policy state: stabilisation windows, cooldowns, redistribution baselines and holds.
- Retained blocked requests such as an unsupported image transition.
- Completion records, including tombstones for fleets that were deleted before they were ever provisioned.

## Invariants

1. **Persist before effect.** Intent is committed with a resourceVersion compare-and-swap on the reservation before any Pod is deleted, any replica count changed or any stop request sent. A crash between the write and the effect resumes the operation; a crash before the write means nothing happened.
2. **Exactly one writer of desired capacity.** Manual `spec.replicas`, the capacity policy and maintenance requests all become journal entries. Only the lifecycle executor changes workloads.
3. **Issued authority survives everything.** An issued removal keeps its authority across leader change, controller restart, pause and spec edits, and must finish recovery. Only unissued actions can be cancelled, and cancellation is itself a durable intent followed by a workload fence.
4. **Status is a projection.** Clearing `status.lifecycle` cancels nothing. `LifecycleProgress` reports a durable transition; the reservation decides.
5. **Absence is not evidence.** Missing metadata, a missing Pod, or an expired lease inferred from wall-clock time never resolves a retirement. Only a positively observed expiry or a signed receipt does.

## Versioning

The journal is at version 8. Readers accept versions 1 through 7 conservatively and rewrite to 8 on the next durable write. An older binary that meets a newer journal refuses to run against it rather than ignoring authority it does not understand, which is why downgrading after an upgrade is not a supported rollback. Each version step and what it added is recorded in the ADRs; the compatibility rules are in [journal archives](../../contracts/journal-archives/).

## Archives

Once the encoded journal exceeds 180 KiB, large fields are paged into immutable, content-addressed ConfigMaps in the fleet namespace, 128 KiB per page, each bound to the reservation UID with a byte length and SHA-256 digest. The reservation keeps a small index. Pages are created and verified before the index CAS publishes them, so a crash can leave orphaned pages but never a partial journal. Hydrated authority is capped at 16 MiB per reservation and fails closed.

Pages have no owner references. Deleting the fleet namespace destroys them and makes the reservation deliberately unusable until exact authority is restored. Back them up with the reservation.

## What you must never do

- Delete a reservation to reuse a bucket or a retained disk.
- Edit reservation annotations to reset a policy hold, clear a loss finding or force progress.
- Remove the deletion finalizer as routine cleanup.
- Roll back to an operator that cannot read the persisted journal version.

Each of these converts an uncertain situation into a silent one. The operator's response to uncertainty is to block for review, and the [safety model](../safety-model/) explains why.
