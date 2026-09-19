# Limitations of the unmodified runtime

The operator must use an existing, unmodified released celld binary. Modifying
celld is explicitly out of scope. The hypothetical runtime contracts below
explain the blockers; they are not planned upstream changes. These limitations cannot be resolved by treating Pod absence, an expired lease, or a
retained PVC as evidence that recovery succeeded.

## All-stopped recovery with unsealed logs

In v0.5.0, the recovery-only follower listener starts concurrently with
`recover_self`; there is no fleet-wide readiness barrier before recovery can
classify expired, unreachable followers as conclusive and record bounded loss.
Starting all Pods in parallel therefore cannot safely resolve this case.

A runtime extension would need a recovery-only startup mode which serves retained
follower fragments without starting writers or adjudicating unavailable peers.
The operator would authenticate each exact disk/invocation and establish the
complete captured follower set before releasing recovery. Release must remain
idempotent across leader and Pod crashes, and missing disks must remain blocked.
Qualification must preserve every acknowledged write across delayed follower
startup, network partitions, operator restart, and failed sealing.

Source: v0.5.0 commit `12d5b6333fe52717325addcfe1e99e9fd4f77bcd`,
`crates/celld/main.rs` recovery-only startup and `crates/celld/node_log.rs`
`recover_self` follower availability handling. The implemented coordinated path
requires every captured own-log obligation already sealed before replacing Pods.

## Bucket metadata collection

The same release's `crates/celld/dead_node_gc.rs` preserves folded log records,
but lines 396–406 CAS a no-log Bucket record into a tombstone and then issue an
unconditional delete. That delete can arrive after a successor installs the same
node key, or remove the old record before the operator observes positive expiry.

The smallest runtime change is to retain the no-log CAS tombstone, just as folded
records are retained. A conditional delete alone prevents erasing a successor
but does not preserve expiry evidence for an operator that was offline. An
alternative is a durable, generation-bound retirement record with equivalent
fencing semantics. Both require adapter qualification against the changed
contract. Test delayed old-generation GC after successor installation and an
operator outage longer than the GC interval; unknown writers must remain refused.

No such upstream change or alternate image is included here. Current logic
retains positive expiry observations and refuses missing-before-proof metadata.

## Legacy persistent storage conversion

An unwrapped process has no authenticated invocation, durable restart denial, or
launcher disk nonce. An RWO claim also cannot be changed in place into RWOP.
A complete migration needs a separately journaled source-host and disk capture,
positive infrastructure fence, retained-PV claim replacement with exact predecessor
and successor claim UIDs, and offline initialization of the launcher identity.
Any unsealed logs still require the recovery barrier above. Recreating an empty
volume, silently adopting an existing claim, or interpreting RWO as RWOP would
break the storage authority contract. This conversion remains unimplemented.
