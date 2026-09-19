---
title: Safety model
description: What the operator accepts as evidence that a writer stopped, what it refuses to infer, and how it keeps unqualified actions blocked and visible.
sidebar:
  order: 4
---

celld can exit with status 0 while durability work is unfinished, and Kubernetes cannot tell the operator whether a process on a lost node is still writing. The operator therefore treats "safe to remove" as a claim that must be proven with evidence bound to the exact session and operation, and blocks when it cannot be. [ADR 0008](../../decisions/0008-recovery-evidence-and-conservative-removal/) and [ADR 0010](../../decisions/0010-production-qualification/) set this posture.

## Evidence the operator accepts

| Evidence | Source | Used for |
| --- | --- | --- |
| Positively observed expired lease with no peer-log obligation | Read-only `nodes/` and `log/` inventory in the fleet bucket | Bucket completion; PersistentFleet retirement admission |
| Sealed own log, positively read | same | PersistentFleet removals, restarts, deletion and upgrades |
| Signed `Stopped` receipt with the restart-denial bit | Launcher on port 8083, HMAC bound to Pod UID, node, host, invocation nonce, generation and operation | PersistentFleet stop certainty |
| Exclusive healthy `ReadWriteOncePod` attachment with unchanged disk identity | VolumeAttachment, PV and Node objects plus the launcher's disk nonce | Graceful cross-node PVC handoff |
| `terminated` from `DescribeInstances` after a `TerminateInstances` the operator issued | EC2, opt-in | Fencing one already-admitted unreachable donor |
| Fresh, complete survivor observation through settling | Pod inventory, `/state`, Metrics Server | Every completion; repeated after ten seconds |

Evidence is persisted in the journal as soon as it is first observed, so a later garbage collection of the S3 record cannot un-prove an expiry.

## What is never evidence

- A missing Pod, a terminated container, or exit code 0.
- Lease expiry computed from time rather than read from S3.
- A retained PVC, a released PV, or a force-detached volume.
- `Ready=True`, a reconciled manifest, or a passing unit test.
- A Pod with the same name but a different UID.
- A timeout, an administrator annotation, or any flag. No attestation bypasses a gate.

## Blocked, and visible

When evidence is missing, stale, ambiguous or unsupported, the operator blocks the specific action, keeps compatible additive capacity working, reports a reason and retains everything. The reasons you will meet most often:

| Reason | Meaning |
| --- | --- |
| `BucketCompletionUnqualified` | No evidence transport: neither the production reader nor the local fixture is configured. |
| `FencingUnqualified` | A PersistentFleet provisioned without the launcher cannot produce stop receipts. |
| `BucketAutomaticUnqualified`, `PersistentAutomaticUnqualified` | Automatic contraction against production evidence is release-gated until EKS and S3 qualification. |
| `FollowerRetirementUnqualified` | A live contraction would leave fewer than two PersistentFleet nodes. |
| `SessionBindingUnqualified` | Observed sessions cannot be bound to admitted generations; unknown historical writers are refused. |
| `StorageIdentityConflict` | A retained claim is missing, replaced or of uncertain provenance. |
| `UnsupportedTransition` | A runtime image change with no qualified adapter pair, including any rollback. |
| `PossibleDataLoss` | A sticky loss finding read from the runtime's declarations; requires investigation. |

The full list is in the [conditions reference](../../reference/conditions/).

## The disruption contract

Applications must expect Cloudflare-style interruptions: cancelled requests, dropped WebSockets, retried work. The operator attempts graceful handoff and preserves acknowledged durable writes within the tested failure model. It does not preserve in-memory state and never replays ambiguous non-idempotent requests. See [ADR 0005](../../decisions/0005-application-disruption/).

## Qualification tiers

Evidence about the operator itself is recorded in tiers, and the site labels it the same way:

| Tier | What it means |
| --- | --- |
| Source-derived | An argument from the pinned celld sources, with commit links. |
| Locally tested | Executed against the released image on Docker or MinIO. |
| Integration-tested | Executed in the disposable kind cluster with Calico, MinIO and local-path volumes. |
| AWS-qualified | None yet. EKS, real S3 and EBS handoff, node failure and partitions are open gates. |

The [qualification index](../../qualification/) records each run, and [implementation status](../../contracts/critical-features/) lists which gates remain.
