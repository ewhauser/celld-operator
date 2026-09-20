# ADR 0012 Journaled manual lifecycle and explicit contraction gates

The production read-only transport, observational history, deadlines and cancellation authority below are updated by [ADR 0015](0015-shared-lifecycle-safety.md). Bucket contraction is subsequently implemented by [ADR 0016](0016-bucket-preflight-and-completion-boundary.md); PersistentFleet remains blocked.

Status: Implemented experimental scale-out; contraction engine tested with injected evidence only

## Authority and journal

Only `spec.replicas` becomes mutable. Placement, identity, storage, profile and
runtime templates remain immutable. There is no scale subresource, capacity
policy, upgrade or cleanup API. Reservations retain the original full spec hash;
the reservation records the original replica count atomically at creation for hash
verification, even before prerequisites or the workload exist. The journal copies
this baseline while recording the actual initially provisioned count separately.

The retained CelldStorageReservation annotation `celld.eric.dev/lifecycle-journal`
is the authority, not fleet status or a leader-election Lease. Version 1 records
workload UID, initial/applied counts, operation UUID, phase, from/to counts,
workload resourceVersion, selected Pod name/UID and runtime generation, complete
session inventory, retained PVC UIDs, settling observation, sticky loss finding and completed operation/target history
with accepted recovery observation timestamps.
Status is an informational projection and can be reconstructed after restart.

Every intent is written with an optimistic reservation Update before any replica
change. Workload Update carries the recorded resourceVersion and operation UUID.
An old leader can only win that exact CAS once. A crash after the effect but before
the journal transition is reconstructed from matching workload UID, operation UUID
and exact replica count. A changed workload version can be durably re-recorded only
while replicas remain at the operation's original count; the obsolete version can
no longer win. Infrastructure comparison remains complete, apart from known API
defaults. Replica drift without matching operation identity is blocked.

## Storage and additive capacity

Scale-out is available for both experimental profiles. It finishes its recorded
additive target even if desired replicas change in flight, then evaluates the new
request. It never turns that change into an implicit contraction. Replica changes
alone do not alter workload templates or rollout strategies.

Before new StatefulSet ordinals can start, atomically create their PVCs and persist
each returned UID. Never adopt an existing deterministic claim, including one with
matching labels. A crash between PVC creation and UID persistence blocks that
operation for investigation and retains the disk. This is deliberately less live
than guessing whether a retained disk is safe. Initial provisioning also records
claim UIDs before creating its workload. Step 2 PersistentFleet reservations
without this UID inventory require manual review; there is no automatic migration
that trusts labels. Bucket reservations can bootstrap a journal after verifying
the original spec hash and complete workload spec.

Claim deletion/replacement/deletionTimestamp blocks lifecycle actions; no PVC,
workload or reservation deletion privilege is added. Retained claims are not
recycled. Reactivating a previously removed runtime identity remains blocked
until retained-disk restart is qualified. Journal growth or annotation limits fail the write before further action;
there is no history pruning or age-based permission to forget recovery evidence.

## Contraction and evidence

**The original step-3 manager could not execute contraction in either profile.** The current Bucket executor follows ADR 0016; the section below records the original qualification boundary. Bucket
completion remains unqualified. Deterministic Deployment victim control is not required when every possible victim passes admission; see [ADR 0016](0016-bucket-preflight-and-completion-boundary.md).
PersistentFleet needs a qualified process fence, live runtime membership collector,
and independently provisioned read-only S3 transport. None is installed by this
change. `--local-test` alone cannot bypass the gate. There is no administrator
boolean that declares an unknown process stopped.

An unexported in-package qualification seam exercises the PersistentFleet engine:

1. Select exactly the highest ordinal, capture Pod UID and exact runtime session,
   preserve every historical session, and persist intent before mutation.
2. Revalidate membership, survivor state and disk identities, run the pinned
   v0.5.0 read-only preflight against complete `nodes/` and historical `log/`
   listings, and revalidate before changing replicas by exactly one.
3. Reconstruct an issued removal after restart and demand an exact process fence.
   Pod absence, Ready, exit 0, zero owned cells and a sealed live log are insufficient.
4. Assess all stopped sessions, exact generations, non-rewound epochs, live leases,
   complete listings and loss declarations. Generation replacement, absent logs,
   lost history and stale/incomplete evidence block. Loss findings are sticky on
   the reservation even if a later listing stops showing the object. Before writing
   the reservation loss finding, CAS a sticky loss fence onto the workload itself.
   This invalidates outstanding replica CASes; a crash before journal persistence
   is reconstructed from the workload fence. If replica issuance wins the CAS
   first, that removal is already issued, and the fence blocks subsequent removals.
5. Revalidate membership/state around assessment, persist the first positive
   observation, and repeat the full assessment at least ten seconds later. This
   experimental settling interval adds a second check; elapsed time never turns
   missing or negative evidence into success. Any assessment error resets it.
6. Retain session history and every PVC UID before permitting another operation.

The seam's Capture/Validate/Stopped implementations are part of the future
qualification trust boundary: Capture must collect current and historical sessions;
Validate must bind exact Pod/container incarnations and fresh `/state` observations;
Stopped must provide real fencing evidence. Tests supply synthetic implementations,
not a Kubernetes or AWS fencing claim. The adapter reads node bodies and lists
names only; no recovery, lease mutation, application data or bundle body access is
introduced. A production S3 transport and IAM restrictions remain unqualified and
unwired. The controller does not accept externally supplied JSON as fencing proof.

A changed desired count never chooses another target mid-operation. Before an
unissued removal, a reversal freezes the existing intent, because a delayed leader
might still be attempting its CAS (superseded by ADR 0015's cancellation fence:
since 19 September 2026 a manual reversal to at least the original count withdraws
the unissued manual removal through the workload CAS, the same path an addition
uses; recorded automatic removals still freeze per ADR 0013). An already-issued removal continues recovery
regardless of the new desired count. Uncertain recovery blocks all further
contraction. Additive requests during recovery are conservatively queued rather
than reusing a removed persistent identity before its recovery is established.

Amendment (20 September 2026): steady-state evidence observation persists on
change or on a one-minute heartbeat, not on every poll. The observer runs on
every reconcile of every fleet, and an idle healthy fleet moves only the
per-session `FirstSeen`/`LastSeen` stamps and the inventory `CheckedAt`, so the
unconditional write was roughly ten reservation writes per minute per fleet that
decided nothing. A write is skipped only when the whole journal, with those
timestamps normalized away, is unchanged, no operation, maintenance, migration
or disruption request is recorded, the fleet is not deleting, and the durable
observation is younger than the heartbeat. Evidence still precedes effect: every
transition writes the whole journal including the inventory, and the five-second
freshness bounds on `Inventory.CheckedAt` compare the in-memory observation with
the clock inside the same reconcile, so a skipped write is one no decision
followed. After a crash the loaded journal carries the older `CheckedAt`, which
fails those bounds and forces a fresh observation before any action, which is the
order this ADR already requires. The heartbeat keeps the
`status.lifecycle.evidenceCheckedAt` projection, which is read back from the
reservation, honest about the age of the last durable observation.

## Limits

The serialization claim covers cooperating controllers and the Kubernetes API's
resourceVersion semantics. Privileged external mutation of workloads, journals or
PVCs is outside it and generally detected as drift. It does not fence ordinary
kubelet/StatefulSet restarts or an unreachable old node. Retained local-path disks
are not EBS qualification. No AWS access, production enablement, automatic
metrics/capacity policy, rolling updates, maintenance or release packaging is added.

## Review fixes

The loss fence is workload metadata, not a Pod-template change. Recording it
retries resource-version conflicts using fresh uncached workload reads and checks
the original workload UID. It preserves current replicas and all other fields.
Issuers never refresh a stale replica write after a fence: the workload marker
is first copied into the reservation journal and remains a contraction blocker.
A reservation read alone would leave the original two-object authorization race.

New reservations store `spec.initialReplicas` together with the immutable spec
hash. Replica edits before initial provisioning therefore neither change the hash
nor require recreating a storage reservation. Existing reservations without this
field retain the prior fail-closed baseline reconstruction; no uncertain legacy
reservation is automatically migrated.
