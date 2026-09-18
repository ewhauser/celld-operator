# Bucket scale-in: preflight and remaining completion boundary

18 September 2026. **Bucket contraction remains disabled for both manual and
Automatic requests.** This change implements Bucket-specific admission checks;
it does not implement or qualify a removal executor. The durable Blocked request,
30-minute deadline and cancellation-before-addition protocol remain in use.
There is no enablement flag, administrative attestation or synthetic production
fencing provider.

## What differs from PersistentFleet

The pinned celld source explicitly configures bucket posture to require object
store durability even when peers exist: [main.rs, lines 4213–4220](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L4213-L4220).
Its acknowledgement path rechecks cell ownership after bucket proof; superseded
writers use their older epoch's objects. See the pinned
[acknowledgement implementation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/actor.rs#L3862).
Consequently, Bucket's acknowledged-write durability does not depend on retaining
a removed node's peer-log disk. An all-Bucket fleet should not need a matching
sealed peer-log record before each removal.

That is a configuration-and-runtime argument, **not an inference from missing
logs**. The same runtime also supports predecessor recovery across postures.
A bucket with peer-log history, an unknown writer, a missing generation or an
unresolved previous session cannot acquire Bucket-only safety merely because its
current Deployment says `CELLD_DURABILITY=bucket`.

Durability and process completion are distinct. A stale Bucket process is subject
to the runtime's ownership protocol, but an absent Pod does not establish that
this process stopped. The stronger completion contract in
[ADR 0015](decisions/0015-shared-lifecycle-safety.md) remains unchanged. The existing
production provider still cannot prove exact irreversible termination and restart
prevention, or authenticate the runtime-generation/container association. This
change does not claim that peer-log durability itself requires those guarantees.

## Implemented admission checks

`InspectBucket` returns a `BucketObservation`, never a peer `Evidence.Completed`
entry. For the narrow current-session case it requires:

- A complete, fresh caller inventory with one current generation per node and no
  stopped or peer-log sessions. Unresolved history remains a blocker.
- A complete node listing, matching node bodies and generations, fresh load
  samples and live leases. Missing records and partial/denied reads block.
- No node peer-log record and no object under `log/`. All listing pages are
  checked, including historical objects. A loss declaration remains separately
  identifiable even when it follows an ordinary log object on a later page.
- An unexpired request budget and noncanceled context.

The production blocked-removal path now checks every matching Pod, because a
Deployment can choose any victim. Each must have an exact Pod-to-ReplicaSet-to-
Deployment controller chain and the expected pinned ReplicaSet runtime template.
The actual Pod invocation and environment must match; duplicate environment
variables, `envFrom`, unknown overrides, extra containers and unqualified runtime
configuration block. Known EKS identity-injection variables are allowed; they do
not establish IAM qualification. The identity, host and address of every candidate
are checked again after metadata reads. Only read access to ReplicaSets is added.

The existing shared metrics preflight projects every possible donor's full demand
onto every possible survivor. No deterministic Deployment victim is required.
Positive no-peer-log observations do not override session-binding, process-fencing,
or executor gates. A newly observed loss acquires the existing durable workload
and reservation loss fence before another operation can proceed.

No journal fields or version change are introduced. Version 4, prior-version
reads, deadlines, cancellation CAS, sticky losses, independent Ready status and
all P2 corrections remain intact. Unknown historical sessions are never pruned.

## Work still required to finish checklist item 2

- [ ] Supply qualified completion/identity authority under ADR 0015, or explicitly
  decide on a different Bucket operation-completion contract. A weaker contract
  would need to distinguish observed replica convergence from actual process
  termination; it is not adopted by this change.
- [ ] Persist the complete all-candidate admission snapshot and execute one
  replica decrement through the shared CAS protocol.
- [ ] Identify every process affected by the decrement, establish completion,
  recheck survivors, and repeat fresh assessments through settling.
- [ ] Define safe disposition of historical Bucket sessions after completion;
  the current narrow preflight intentionally rejects unresolved old sessions.
- [ ] Exercise manual and automatic repeated shrink/grow, crashes at each new
  execution phase, competing membership changes and generation replacement.
- [ ] Qualify acknowledged-write preservation and availability using real runtime
  workloads, then the EKS/S3 environment. A local ledger is bounded evidence,
  not AWS qualification or proof for every dormant cell.

A successful preflight alone authorizes no replica change. Subsequent additions
can still supersede an unissued blocked removal using the existing cancellation
protocol. Issued removals, if encountered from prior authority, remain blocked on
recovery; they cannot be silently archived as complete.
