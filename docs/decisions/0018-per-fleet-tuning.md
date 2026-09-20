# ADR 0018 Per-fleet execution and lifecycle tuning

Status: Implemented

Date: 19 September 2026

## Context

The design proposal sized the celld container and bounded its residency and
shutdown per fleet (`execution` and `lifecycle` blocks). The operator shipped
fixed constants instead: 250m CPU request, 512Mi/1Gi memory, no CPU limit, a
20 second runtime stop budget inside a 30 second pod grace, and no residency
cap or idle eviction. The pre-release review recorded this as a gap.

## Decision

Add optional `spec.execution` (`cpuRequest`, `cpuLimit`, `memoryRequest`,
`memoryLimit`, `maxResidentCells`, `idleEvictSeconds`) and `spec.lifecycle`
(`shutdownSeconds`, `terminationGraceSeconds`). Omitted fields keep the exact
historical constants, so an existing fleet's generated template and reservation
spec hash are byte-identical to before; only fleets that set a block get a
different template and a different hash.

Both blocks are **immutable after creation**, enforced by CRD CEL and mirrored in
controller validation. The operator never updates a workload template (ADR
0011), so a mutable tuning field would either be silently ignored or read as
drift. Resizing a fleet therefore means a new fleet with a new bucket, or a
future qualified migration, exactly as for storage and placement.

Cross-field rules: limits must be at least requests; `terminationGraceSeconds`
must exceed `shutdownSeconds` by at least five seconds, leaving room for signal
delivery and the launcher's inherited-lock proof. When the grace is not the
default, the launcher is told the grace itself and derives both of its
unrequested-termination waits from it: the SIGTERM-to-SIGKILL wait plus the
inherited-lock proof plus a five second margin equal the grace, so the whole
sequence finishes before kubelet's SIGKILL (15 s + 10 s + 5 s at the default
grace of 30; the lock proof is capped at ten seconds, and longer graces go to
the SIGTERM wait). A stop the controller requested is not racing kubelet and
keeps the whole grace minus the margin for the child, which the cross-field rule
above makes at least `shutdownSeconds`; that wait must never be the shorter
unrequested slice, or the launcher kills the runtime part way through its own
graceful shutdown. The v0.4.1 drain-token wait follows the shutdown budget.

The design's requirement of equal requests and limits is not enforced: the
historical default has no CPU limit and a memory limit above its request, and
changing that would alter every existing template. Equal values are the
recommended starting point for new fleets, not a rule.

## Consequences

Resident-cell caps, eviction ages and shutdown budgets are per-application
measurements; the sample values are not recommendations. A longer shutdown
budget is an opportunity to hand off, not proof of durability. The capacity
policy's CPU and memory thresholds remain absolute and are not normalized to
requests. Ephemeral storage still follows `storage.sizeGiB`.
