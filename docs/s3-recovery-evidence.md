# Read-only S3 recovery evidence

Current implementation: [shared lifecycle safety](shared-lifecycle-safety.md). Production collection is now wired; exact identity/fencing and mode-specific removal qualification remain blocked.
Implementation follow-up: [step 1 adapter, real local experiments, and remaining release gates](qualification/README.md). The earlier source conclusions below are historical; the follow-up distinguishes observations from unqualified hypotheses.

18 September 2026. Proposed contract for celld v0.5.0, commit `12d5b6333fe52717325addcfe1e99e9fd4f77bcd`, following [shutdown evidence](shutdown-evidence.md). The user has accepted narrowly scoped read-only S3 metadata access. This is a source-backed design for qualification, not an implemented controller or an AWS-tested safety guarantee.

## Decision

Read celld's published node-log state and loss-declaration names. Keep recovery, follower selection, fencing, data upload, and record mutation inside celld. This adds a small but real dependency on a private, version-specific storage schema; it is not a generic stable celld API.

The new access removes the HTTP-only observability blocker in principle: the operator can observe celld's authoritative recovery outcome instead of guessing from process exit or elapsed time. Repeated peer-disk contraction still needs failure testing, correct session tracking, and lifecycle serialization. It does not establish AZ-loss durability or solve pressured startup.

## Important correction to the earlier reports

**Sealed does not, by itself, mean lossless recovery.** If an active log has no complete surviving follower witness and every member's fate is conclusive, celld writes `log/<node>/<generation>.e<epoch>.loss.json`, then continues recovery and sealing. Corrupt retained bundles can likewise produce `log/<node>/<generation>.bundle-<name>.loss.json`. These are possible-loss declarations, not proof of an exact number of lost acknowledged writes.

The loss declaration is written before the recovery proceeds. The operator must detect it and block controlled removal, including when the lease has already become sealed or a successor generation has replaced it. Do not simply port the runtime's `takeover_gate` decision into an operator `SafeToRemove` predicate.

Sources: [witness-loss branch](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L4696-L4747), [bundle-loss declaration](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L5580-L5642).

## Minimum S3 surface

All paths are relative to the fleet's configured storage prefix.

| Surface | Purpose | Required access |
| --- | --- | --- |
| `nodes/<node>.json` | Exact node identity, process generation, lease expiry, folded `log.state`, epoch and ensemble | `s3:GetObject` |
| `nodes/` | Inventory historical and current node records, including dead nodes unknown to a newly started operator | Prefix-restricted `s3:ListBucket` |
| `log/` | Detect any `*.loss.json` declaration, including past generations | Prefix-restricted `s3:ListBucket`; object names suffice to block |

No reads of cell databases, WAL/bundle contents, deployment secrets, or application payloads are needed. No S3 write/delete permission is needed. Listing `log/` reveals object names and sizes for bundles as well as loss declarations; S3 does not offer a suffix filter to restrict listing to `.loss.json`. Use the runtime's hierarchy and delimiter pagination where useful, but inspect declaration-bearing levels completely. Do not describe this as visibility limited exclusively to loss records.

The wire generation is `ownership_index_generation`, falling back to `probe_public_key` when empty; validate it and node identity rather than silently accepting missing fields. `log` can be absent before this session's first peer-durability log. `open`, `recovering`, and `sealed` are the known states. Unknown versions, malformed records, inconsistent identities, or incomplete pagination block contraction. A JSON null or absent field has meaning only under this exact qualified schema.

Sources: [lease wire and generation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/ownership_store.rs#L34-L71), [decoder](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/ownership_store.rs#L179-L246), [folded reader](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L543-L565).

## Proposed conservative sequence

1. Before each controlled disruption, validate the pinned runtime profile and fleet storage identity. Enumerate node records and reconcile them with known pod/container incarnations and stable node identities. Persist the candidate's generation, relevant epochs, pod UID, container identity, and operation ID before changing replicas. Unknown writers, lost identity history, or unresolved unavailable nodes block a new removal.
2. Require all previously stopped or unavailable sessions to have an acceptable recovery outcome, and require a complete loss-declaration check. Keep a durable unresolved-session ledger so deleting a pod or restarting the operator cannot erase a blocker. Do not require every live leader's log to be sealed: normal peer-disk operation has open logs. A live leader that becomes unavailable invalidates the current assessment.
3. Apply one gated removal and retain its PVC. Do not count disappearance of a Pod object alone as proof that a process on an unreachable node stopped. Force deletion and node partitions require separate fencing/recovery treatment. Preserve access to surviving follower processes while celld recovers.
4. Poll the exact removed session's record. An `open` or `recovering` log remains blocked. For a confirmed stopped session, a matching `sealed` record is a candidate completion signal. Read it directly from the primary S3 bucket, not a capacity cache, replica bucket, or event feed.
5. **After observing completion**, perform the loss-declaration check, including all relevant historical sessions. For a conservative first implementation, any loss declaration anywhere in the fleet blocks further controlled disruption and raises a durable condition for investigation. Do not automatically clear it based on age or silently acknowledge it at controller startup.
6. Revalidate membership, pod health, full fresh metrics, and the desired count, then finish the recovery/settle phase. Persist the accepted evidence and retain PVCs. A later runtime restart or new unavailability requires another assessment; a seal observed while a process was still live is not a reusable certificate because a live session can open a later epoch.

Source-derived special cases should initially fail closed until tested: a changed generation can mean recovery-before-install, but requires binding the old session and checking historical loss records; an absent `log` can mean no peer acknowledgements were issued, but requires a confirmed stopped, known session; a missing node object must not silently erase a previously observed open log. In this release, folded log records are retained as sealed tombstones rather than deleted. A missing such record is anomalous.

Sources: [recovery before successor install](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L4255-L4300), [dead-record tombstone](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/dead_node_gc.rs#L356-L416), [reopening and reconfiguration](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L5195-L5265).

S3 provides strong read/list consistency, but not an atomic snapshot across all keys. Reading the completion record before checking declarations matters because celld publishes declarations before completing recovery. Fully paginate and reject partial results; revalidate lifecycle and membership around the assessment. No sequence of reads can prevent an independent node failure immediately after the check. Claims remain bounded by the qualified failure model. The storage scope must exclude external deletion/rewind of recovery evidence, including lifecycle expiration of these records.

## IAM and operation cost

Use an externally provisioned read-only role for the operator, separate from celld's storage-writing role. Grant `s3:GetObject` only on `arn:aws:s3:::BUCKET/FLEET_PREFIX/nodes/*`, and `s3:ListBucket` on the bucket with `s3:prefix` restricted to `FLEET_PREFIX/nodes/`, `FLEET_PREFIX/nodes/*`, `FLEET_PREFIX/log/`, and `FLEET_PREFIX/log/*`. This is a permissions outline, not a deployed policy. If node records use SSE-KMS, separately qualify the KMS decrypt permission required to read them, with the appropriate key policy and scope. Explicit region/bucket configuration avoids needing broad discovery permissions.

Perform metadata assessment around disruptions and recovery, not as a replacement for HTTP/metrics sampling on every policy tick. Full historical loss discovery can become expensive with many generations or bundles. Measure delimiter-based traversal and pagination costs; an incomplete or budget-exhausted scan holds contraction. Never trade away completeness for a best-effort list. Persist found declarations without retaining their potentially sensitive bodies.

AWS references: [consistency](https://aws.amazon.com/s3/consistency/), [data consistency model](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel), [prefix-restricted policies](https://docs.aws.amazon.com/AmazonS3/latest/userguide/amazon-s3-policy-keys.html), [GetObject permissions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html).

## Required qualification before production

- Normal sequential removal of multiple nodes, proving each matching stopped session completes and the client acknowledged-operation ledger survives.
- Exit code 0 with an open log; verify the next removal remains blocked until runtime recovery completes.
- A sealed log accompanied by either type of loss declaration; verify no false success. Test old generations and declarations discovered after operator restart.
- Leader loss, stalled S3, AccessDenied, truncated listings, controller crashes, generation replacement, and delayed status writes around every transition.
- Missing metadata after a previously observed open log, unknown schemas, and concurrent node partition/forced pod deletion; all remain fail-closed.
- IAM isolation: node JSON reads and permitted lists succeed; application objects, bundle bodies, unrelated fleet prefixes, writes, and deletes are denied. Test SSE-KMS if used.
- Retained-disk recovery after the removed process had served as a follower for another failed node; retain all required disks and verify the availability and durability limits explicitly.

No new runtime or AWS experiment was run for this document. The implementation recommendation is to qualify this read-only adapter next, not to declare full peer-disk autoscaling safe from source inspection alone.
