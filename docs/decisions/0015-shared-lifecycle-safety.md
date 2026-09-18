# ADR 0015 Shared evidence, deadlines and cancellation authority

Status: Implemented shared prerequisites; identity/fencing and mode-specific executors blocked

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
