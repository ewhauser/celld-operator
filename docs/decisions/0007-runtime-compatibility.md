# ADR 0007 Runtime compatibility boundary

Status: Accepted

Date: 2026-09-18

## Context

The operator must work with existing celld behavior. Requiring new upstream status endpoints is not an acceptable prerequisite for the first release.

## Decision

Use an existing, unmodified celld release. Pin the runtime image and explicitly qualify the HTTP and storage metadata semantics used by the operator. Keep version-specific interpretation inside a runtime adapter.

Do not reimplement cell ownership, lease mutation, follower recruitment, recovery, or handoff. A read-only metadata adapter is permitted under [ADR 0008](0008-recovery-evidence-and-conservative-removal.md).

## Consequences

The explored candidate is v0.5.0 at commit `12d5b6333fe52717325addcfe1e99e9fd4f77bcd`, with image `ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8`. These are recorded candidate pins from the investigation, not production approval.

Unknown required semantics block dependent automation. Every runtime update needs adapter review and a supported transition policy; a rollback is also a transition. Do not assume an arbitrary image update is rolling-safe.

A launcher that enforces restart timing may be investigated without modifying the celld binary, but its signal forwarding and fencing behavior must be qualified. See [runtime qualification](../runtime-qualification.md).
