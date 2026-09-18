# ADR 0002 Durability modes and scaling scope

Status: Accepted

Date: 2026-09-18

## Context

Applications need a choice between bucket acknowledgement latency and peer-disk acknowledgement latency. Supporting only bucket durability or making peer-disk mode manual-only would not satisfy the requested first-release scope.

## Decision

Support both Bucket and PersistentFleet durability profiles. Both are in scope for automatic scale-out and automatic scale-in. Configure celld's durability explicitly; workload selection must not silently change acknowledgement semantics.

Use the design's Deployment with disk-backed ephemeral storage as the Bucket implementation baseline, and a StatefulSet with retained EBS-backed PVCs as the PersistentFleet baseline. Qualify stable node identity and reuse of the correct disk before enabling the persistent profile.

## Consequences

Both profiles must pass their own failure and scaling qualification. Accepted scope is not a claim that repeated peer-disk contraction already works safely.

Block removals when safety cannot be established, as specified in [ADR 0008](0008-recovery-evidence-and-conservative-removal.md). Preserve persistent volumes during contraction and deletion; cleanup is a separate maintenance concern.

Do not silently reduce the first-release scope to one durability mode. If qualification makes the agreed scope infeasible, surface that limitation explicitly.
