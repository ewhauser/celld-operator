# ADR 0015 Shared evidence, deadlines and cancellation authority

Status: Implemented shared prerequisites; Bucket completion amended by ADR 0016; graceful launcher-managed PersistentFleet amended by ADR 0017

Use the production read-only primary S3 transport and persist observational
session history without promoting observations into fencing certificates.
Require fresh full survivor observations and conservative donor projection.
Persist operation deadlines and stalled status, and recover issued effects
regardless of timeout. Allow compatible additions to replace unissued removals
only after a durable cancellation intent and workload CAS fence. Preserve issued
removal authority and unresolved historical sessions.

Advance the journal to version 4, retaining reads of versions 1–3. Existing
version-3 redistribution holds and all P2 corrections are retained. Unsupported
runtime identity/fencing paths remain visibly blocked. No flag or Kubernetes
status inference authorizes production contraction.

See [the implementation contract and exact external dependencies](../shared-lifecycle-safety.md)
for evidence semantics, scope, interleavings and qualification limits.

[ADR 0016](0016-bucket-preflight-and-completion-boundary.md) adopts Bucket logical membership completion, replaces its physical-process-termination requirement, and advances the journal to version 5. The stronger peer-log and fencing contract above continues to apply to PersistentFleet.

[ADR 0017](0017-persistent-launcher-and-graceful-retirement.md) supplies a trusted launcher protocol for the restricted same-host graceful path. Uncertain physical/node failure remains blocked.
