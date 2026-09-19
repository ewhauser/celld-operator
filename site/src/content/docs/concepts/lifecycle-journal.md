---
title: Lifecycle journal
description: Why a fleet can recover an interrupted scale or maintenance operation without forgetting prior writers.
sidebar:
  order: 3
---

Scaling and maintenance can outlive a controller Pod. Before the operator changes a replica count, deletes a Pod or asks a launcher to stop, it records the operation in the fleet's CelldStorageReservation. A new leader resumes that record after a crash. The reservation is cluster-scoped and permanently binds the dedicated bucket to the fleet UID.

Status is a view of this record. Editing or clearing status.lifecycle does not cancel an operation.

## What persists

The journal records the current action and phase; which effects were issued; exact Pod, generation, volume and claim identities; stop receipts and loss fences; capacity-policy holds; and prior admitted sessions. Once an action has been issued, it must finish recovery even if the user pauses the fleet or edits its desired replicas. An unissued action may be cancelled through a durable cancellation path.

This matters most when a member is missing. Absence alone cannot prove that its prior process stopped writing. The operator keeps the prior generation in history and waits for positive recovery evidence. If evidence is incomplete, it blocks the destructive step and reports a condition. See [safety model](../safety-model/).

## Archives and backup

Large journals move fields into immutable, content-addressed ConfigMap pages in the fleet namespace. The reservation retains the index and verifies page digests before using it. Back up the reservation **and** its archive pages together. Losing the namespace can lose pages needed to interpret the permanent reservation. Do not delete a reservation to reuse a bucket or hand-edit annotations to clear a hold.

The current journal format is version 9; supported older versions are read conservatively and rewritten on the next durable update. Rolling back to an operator that cannot read the newer journal is unsupported. See [journal archives](../../contracts/journal-archives/) and [compatibility](../../reference/compatibility/). The [restart-safe lifecycle decision](../../decisions/0012-restart-safe-manual-lifecycle/) explains the design history.
