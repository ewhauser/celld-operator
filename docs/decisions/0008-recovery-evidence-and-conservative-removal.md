# ADR 0008 Recovery evidence and conservative removal

Status: Accepted

Date: 2026-09-18

## Context

Released HTTP status and container exit codes do not establish completion of peer-log recovery. Celld can exit successfully with unfinished durability work. The user accepted narrowly scoped read-only S3 metadata access to address this limitation while keeping celld unmodified.

## Decision

Require a version-pinned, read-only S3 metadata adapter for peer-disk automatic scale-in. Read node records under the fleet's `nodes/` prefix and list metadata under `nodes/` and `log/` to discover relevant sessions and loss declarations.

Bind evidence to the exact runtime session and Kubernetes lifecycle operation. Check recovery completion and possible-loss declarations before authorizing further removals. **A sealed log alone is insufficient:** celld can declare possible loss and then finish sealing.

On missing, ambiguous, stale, or unsupported evidence, block further controlled removals, report the reason, and retain EBS volumes. Apply equivalent safety gates to upgrades and planned restarts. Compatible additive capacity may continue.

## Consequences

The operator receives a separate, externally provisioned read-only identity. No S3 writes, application-data reads, or bundle-body reads are required for the proposed contract. Listing `log/` also reveals bundle object names; it cannot be restricted by a server-side suffix filter to loss records only.

This supersedes the earlier planning preference that the operator require no S3 data access. It permits reading runtime metadata, not performing recovery or rewriting leases.

The detailed proposed read ordering, session ledger, IAM scope, failure rules, and tests are in [S3 recovery evidence](../s3-recovery-evidence.md). They still need qualification; this ADR does not certify them as a complete safety proof.
