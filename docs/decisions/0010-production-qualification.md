# ADR 0010 Production qualification

Status: Accepted

Date: 2026-09-18

## Context

The architecture depends on runtime behavior that source inspection alone cannot establish under Kubernetes failures. The agreed scope includes both durability profiles and both scaling directions.

## Decision

Begin the implementation plan with qualification of the pinned runtime. Gate production enablement on evidence for:

- Repeated peer-disk contraction using S3 completion and loss evidence.
- Added pods becoming useful while existing nodes are pressured.
- Stable node identity, lease-aware restart timing, and retained-EBS recovery.
- Restart-safe controller operations that do not cause duplicate removals.
- Acknowledged-write preservation and application recovery within each profile's tested failure model.

Keep unqualified or uncertain actions blocked and visible. A reconciled manifest, successful exit, or passing source-level test is not production qualification.

## Consequences

Use client-side acknowledged-operation ledgers and inject failures around lifecycle transitions. Measure joining, shutdown, recovery, and service interruption rather than relying only on reduced average CPU.

Resource sizing, scaling thresholds, replica floors, and timing values are configurable starting points until benchmarks justify them. The original proposal's numeric examples are not capacity guarantees.

Record evidence separately as source-derived, locally tested, integration-tested, or AWS-qualified. No AWS qualification has been performed in the investigations linked from [the decision index](README.md).
