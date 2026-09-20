# ADR 0021 Lifecycle state tiers: bounded hot authority, externalized resolution ledger

Status: Proposed

Date: 20 September 2026

## Context

The lifecycle journal (ADR 0012, extended by ADRs 0015 through 0020) is a single
JSON document on the cluster-scoped `CelldStorageReservation`, paged into
immutable ConfigMaps above 180 KiB and hard-capped at 16 MiB hydrated
(`docs/journal-archives.md`). It mixes three kinds of state with different
lifetimes:

1. **Hot authority**: the in-flight operation, maintenance, request, migration,
   capacity window, loss fence, workload UID, runtime image, applied count and
   retained claim UIDs. Bounded by replica count. Read on every reconcile.
2. **Resolution evidence**: per-invocation records in `PersistentHistory`,
   `BucketHistory` and `Inventory.Sessions`, appended on every pod or launcher
   invocation and never removed. Unbounded.
3. **Audit**: `History`, completed `InfrastructureFences`, `CompletedRestarts`.
   Only the last `History` entry has a reader.

A field-by-field trace of every reader (20 September 2026) established:

- `History` beyond its last element, `InfrastructureFences` after their
  operation completes, `Sessions` (never written on a production build),
  `Inventory.CheckedAt`/`Blocker` as durable values, and the
  `creation-claim-uids` annotation after bootstrap have **no reader**.
- `Initial` duplicates `spec.initialReplicas`; `Loss` duplicates the workload
  loss fence; `Request` is recomputable from the fleet spec except its UUID.
- Resolved `BucketHistory` and `PersistentHistory` entries **are** read, in
  five distinct decisions. Their only durable purpose is to classify a runtime
  `nodes/<node>.json` record, or a session observation, as *ours and resolved*
  rather than *unknown writer*. The runtime may garbage-collect that record at
  any time, and absence is ambiguous (ADR 0016), so the classification cannot
  be rebuilt from S3. The full 844-byte persistent record is also read for the
  **latest** entry per node, to authorize cross-host reactivation and handoff.
- The journal is hydrated three to four times per reconcile (`migrateBucket`,
  `reservationMatches`, `lifecycle`, and every `report`). For a paged journal
  that re-reads every ConfigMap page each time.
- The runtime retains folded node records as sealed tombstones rather than
  deleting them (`docs/s3-recovery-evidence.md`), so the per-reconcile
  inventory scan already performs one `GetObject` per invocation the fleet has
  ever had. The S3 side grows exactly as the journal does.

Growth is roughly 844 bytes per launcher invocation and 418 bytes per Bucket pod
invocation, plus 443 bytes per observed session epoch. A ten-pod fleet under a
capacity policy that cycles ten times a day reaches the 180 KiB paging
threshold in weeks and the 16 MiB cap in about a year. At the cap the journal
can no longer be written and the fleet fails closed with no recovery path other
than manual annotation surgery. Nothing warns before that.

## Assumptions revisited

**The journal is the authority and status is a projection.** Still correct.
Nothing here changes the CAS commit point for in-flight intent.

**Never prune; absence never resolves.** Half right. The *fact* that must never
be forgotten is "the operator positively observed retirement of this exact
generation". The *record* does not have to live in etcd, and it does not have to
be consulted except when a runtime record or a session observation names that
generation. Dropping a resolved entry outright would make a still-present
runtime record an unknown writer, which blocks. That direction is conservative,
but it is also permanent, so entries must be kept somewhere for as long as the
runtime keeps its tombstone.

**The reservation must survive fleet deletion, so it is cluster-scoped.** Still
correct for claim UIDs, workload UID, loss and the latest retired disk identity.
But the archives that hold the bulk of the journal are namespaced, so namespace
deletion strands a large journal while a small one survives. The asymmetry is a
consequence of the archive mechanism, not of the reservation.

**The operator is read-only on S3 (ADR 0008).** This was chosen to bound blast
radius on runtime data, and that goal still holds. It does not require the
operator to have no write access to a prefix the runtime never reads. The
operator already cannot act when S3 is unavailable, so a write to the same
bucket adds no new availability dependency.

**Everything must live in one CAS object.** Only hot authority needs
compare-and-swap. Resolution evidence is write-once and immutable. It needs
integrity and non-deletion, which content addressing already provides, not
optimistic concurrency.

**Load then decide, every reconcile.** Hydrating the journal several times per
pass is an implementation artifact, not a design requirement.

## Decision

Split lifecycle state into three tiers with explicit lifetimes.

### Tier 1: hot authority, on the reservation, bounded

Keep on the reservation annotation, under the existing CAS, only state that a
decision reads on every reconcile or during an operation:

- `Version`, `WorkloadUID`, `RuntimeImage`, `Applied`, `Loss`, `Claims`.
- `Operation`, `Maintenance`, `Request`, `BucketMigration`. A completed
  migration collapses to its terminal phase and target UID.
- `Capacity` window counters and per-identity stamps. Drop `Decision`, which is
  pure status projection.
- `InfrastructureFences` only while their operation exists.
- `CompletedRestarts` as the set of tokens still present in the spec, or the
  last completed token if reverting to an earlier token is disallowed.
- `History` as its last entry only.
- `Inventory.CheckedAt` for the status projection. `Blocker` is recomputed each
  pass and is not persisted.
- The **working set**: unresolved sessions and unresolved history entries, plus
  the **latest** persistent member per node with its full disk and host identity.

Everything in this tier is bounded by replica count. Target size is below the
32 KiB inline threshold so the archive path is never taken for hot state.

### Tier 2: resolution ledger, immutable, per generation

At the moment a writer becomes resolved (positive stopped receipt with restart
denial, positively observed lease expiry, or supersession), write one immutable
record keyed by `(node, generation)`:

```
{ node, generation, epoch, outcome, observedAt, receiptDigest }
```

`outcome` is one of `stopped-denied`, `expiry-observed`, `superseded`. `epoch`
is retained because the shutdown path reads a prior entry's epoch to decide
whether a stopped writer legitimately has no sealed log. `receiptDigest` binds
the compact record to the full receipt kept in tier 3.

The ledger is consulted only when classifying a runtime record or a session
observation. A record is *resolved* if and only if a ledger entry exists for its
exact generation and no later contrary observation has invalidated it.
Invalidation (a renewed lease after observed expiry) writes a new record with
outcome `invalidated`, which takes precedence; the ledger is append-only and
never edited.

The ledger lives in the fleet's S3 prefix under `operator/resolved/`, written
with conditional `PutObject` (`If-None-Match: *`) so a record is write-once.
IAM grants the operator `PutObject` on that prefix only, no delete and no
overwrite. The runtime never reads the prefix. The bucket's retention policy
must exclude these objects from lifecycle deletion, which the evidence contract
already requires for the runtime's own records.

Reads of the ledger are gated by the inventory listing: `ListObjectsV2` returns
each key's ETag, so a runtime record whose ETag is unchanged since it was
classified needs neither a `GetObject` nor a ledger read. This bounds the
per-reconcile scan by live and changed records instead of by history.

### Tier 3: audit, no reader

Full retirement receipts, completed fence receipts and completion history are
emitted as Kubernetes Events and structured logs at the moment they are
produced, and stored as immutable objects under `operator/receipts/` in the
same prefix. Nothing in the reconcile path reads them.

### Load once

The reconciler hydrates the journal once per reconcile and passes it to
migration matching, reservation matching, the lifecycle run and the status
report.

### Migration

Journal version 9. On first load of a version 8 journal:

1. Emit a tier-2 record for every resolved `BucketHistory` and
   `PersistentHistory` entry and every non-current `Inventory.Sessions` entry
   that has a resolved counterpart. Emit tier-3 receipts for the full records.
2. Read every emitted record back and verify its digest.
3. Rewrite the reservation with tier-1 content only, under CAS, retaining the
   latest persistent member per node and every unresolved entry.
4. Leave existing archive ConfigMaps in place; they become unreferenced. A
   separate maintenance command may delete unreferenced pages whose reservation
   no longer indexes them.

Version 8 remains readable. Version 9 is rejected by older binaries, as every
previous advance has been. A fleet whose emission or verification fails stays on
version 8 and reports the failure; nothing is dropped before it is verified
elsewhere.

## Alternatives considered

**Compaction inside Kubernetes only.** Keep the current archive mechanism but
store resolved entries as the compact tuple and full records only for the latest
member per node. This bounds the hot journal and cuts growth by roughly five
times, but the ledger still grows without bound in namespaced ConfigMaps, the
namespace-deletion asymmetry remains, and every paged load still re-reads every
page. It is the right first step if the S3 write grant is refused, and it is
phase 2 of the rollout below in any case.

**Retire ledger entries once the runtime record is observed absent.** Once a
record has been garbage-collected while its entry was resolved, the entry can
only ever say "ignore me", and a reappearing record would become an unknown
writer, which blocks. This is safe and strictly conservative. It is not
sufficient on its own because the pinned runtime retains tombstones
indefinitely, so the trigger rarely fires. Adopt it as a rule so the ledger
shrinks if a future runtime does collect tombstones.

**Trust the launcher's on-disk retired marker.** The marker proves a denial was
written, not that the process exited, and it is unreachable once the pod is
gone. ADR 0017 is explicit that markers never recreate certificates. Rejected.

**Move the journal to fleet status.** Status does not survive fleet deletion
and is not the CAS anchor for the workload. Rejected, as in ADR 0012.

**Watches instead of polling.** Orthogonal. It would reduce the number of
reconciles but not the state each one carries. Not addressed here.

## Consequences

- The reservation carries kilobytes, not megabytes, regardless of fleet age.
  The 16 MiB cliff and the archive path disappear for hot state.
- Per-reconcile cost stops growing with history on both the etcd and S3 sides.
- The operator gains a narrowly scoped S3 write. ADR 0008's "no S3 writes"
  consequence is amended to "no writes to runtime-owned keys". IAM, bucket
  policy and retention must be qualified for the new prefix before production
  enablement, and the qualification checklist in
  `docs/s3-recovery-evidence.md` gains write-denial tests for runtime keys.
- A failed ledger write blocks the resolution it would have recorded, the same
  way a failed evidence read blocks today. Nothing is resolved in memory only.
- Journal version 9 is a one-way advance per fleet. Rollback to a version 8
  binary after migration is unsupported, consistent with earlier advances.
- The never-prune rule of ADR 0012 is amended: evidence is never forgotten, but
  it is stored where it is classified and in the form its readers need.

## Rollout

1. **No design change**: hydrate once per reconcile; stop persisting the
   fields with no reader; ETag-gate the inventory scan; publish journal size
   and archive page count as metrics with a warning condition at half the cap.
2. **Compaction**: compact resolved entries in place, keep the latest persistent
   member per node in full, adopt the retire-on-observed-absence rule. Journal
   version 9, still entirely in Kubernetes.
3. **Externalize**: add the S3 ledger and receipts prefix, migrate resolved
   entries out, and stop paging hot state. Requires the IAM change and its
   qualification.

Phases 1 and 2 are safe to implement now. Phase 3 needs the IAM decision.
