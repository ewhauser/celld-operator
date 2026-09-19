# Maintenance execution

The shared retained reservation journal now executes same-version restart tokens
and RetainData deletion. The adapter image remains the pinned v0.5.0 digest.
Different-image upgrades and rollback are **not implemented**: neither a second
adapter nor a qualified directional compatibility contract exists. An arbitrary
new digest remains `UnsupportedTransition`; changing it never edits a pod image.

## Planned restart

A restart token captures the initial exact pod UIDs. Each target gets a durable
irreversible authorization before its UID-preconditioned deletion. The controller
waits for recovery and ten seconds of continuously valid settling before
admitting the next target. A completed token is retained and cannot replay.
Changing/removing the token or requesting deletion cancels only the next action.
Pause prevents new admission, but already authorized actions retain authority
across old-leader requests and crashes and must finish recovery.

Bucket replacement uses the existing immutable no-log posture, complete session
inventory, positive old-lease expiry, survivor health and capacity, and loss scan.
A replacement with the same Kubernetes name but a different UID is never deleted
by an old authorization. Current conservative placement admission can still block
legacy Deployment restarts if every possible removal is not safe. Ordered
restart additionally checks its actual target, rather than the highest ordinal
used for contraction: three nodes across two strict AZs cannot restart the sole
node in one AZ. Supply balanced extra capacity and a new token, or configure
Relaxed placement up front, to permit that disruption.

Launcher-managed PersistentFleet first stops the exact child, requires its
authenticated `RestartDenied` receipt backed by the durable per-pod deny marker,
and verifies sealed
recovery and departing-follower tiering using the existing survivor protocol,
retains the predecessor identity, and then deletes only that pod UID. Replacement
must use the same PVC/PV and establish an exclusive new invocation before the
controller proceeds. At least two survivors are still required. Cross-host
restart additionally requires authenticated disk nonce continuity, the same AZ,
RWOP/EBS attachment evidence, and launcher handoff authorization before celld
starts. Local hostPath integration exercises same-host reuse only; AWS handoff
remains a separate qualification gate.

## RetainData deletion

Bucket deletion captures every live and historical writer under the no-log
contract, durably authorizes zero replicas, and requires empty Kubernetes
membership plus positively expired leases for every admitted generation. A full
loss scan and repeated settling assessment precede workload deletion and finalizer
completion. S3 data, PVCs/PVs, the permanent reservation and recovery journal are
never deleted. Network isolation and credentials remain retained as well.

PersistentFleet deletion captures all launcher invocations and log epochs, stops
each authenticated child, then requires **every captured leader's own log sealed**,
no epoch rewind or missing metadata, expired leases and no historical loss.
Nodes with no own log can pass only when their captured/current epochs are zero
and no positive epoch exists in retained history; all other leaders must still
seal their own logs. This exemption applies only to the all-stopped shutdown. A
single sealed departing node cannot certify another leader's follower obligations.
All exact stop receipts are rechecked before zero replicas. Only then can compute
cleanup and finalizer completion occur.

**Qualification limitation:** pinned source `12d5b6333fe52717325addcfe1e99e9fd4f77bcd`
implements its own graceful seal: `node_log.rs:4970-5061` waits for tiering and
requires retained bundle coverage and per-cell tails before sealing; shutdown
calls it from `main.rs:5420-5435`. This does not inherently require a live survivor.
It is best effort: timeout, uncovered rows or failed metadata writes leave Open.
An all-stopped fleet can therefore remain blocked with disks and workload intact.
Unit evidence validates refusal and the all-sealed decision rule. Real-runtime
positive shutdown, timeout recovery and fault qualification remain required;
exit code zero alone never establishes the seal.

Before any workload creation attempt, deletion installs a permanent reservation
tombstone and completes without inventing runtime evidence. Once a workload
creation attempt exists, an absent workload does not establish termination:
finalization requires the durable Cleanup phase. The tombstone fences delayed
creation updates and remains after the CelldFleet disappears.

## Remaining migrations

Existing workloads without the launcher and existing RWO PVCs are not silently
adopted or edited. Introducing the launcher or changing PVC access modes requires
an explicit migration protocol and qualified exclusive handoff; this change does
not claim to implement those migrations. AWS fault qualification, the final
PersistentFleet protocol above, and directional cross-version adapters remain
separate requirements.

## Coordinated downtime for small PersistentFleet operations

Set `spec.maintenance.allowCoordinatedDowntime: true` to authorize an intentional
whole-fleet outage for a manual 2-to-1 contraction, or a same-version restart of a
one- or two-member fleet. The default remains conservative; automatic capacity
never selects this protocol. A target of one requires `placement.azCount: 1`.
The existing live contraction/restart path still requires two survivors.

The durable sequence captures all exact invocations and retained disk identities,
stops every child with its fsynced per-Pod restart-denial receipt, and verifies
all positive own-log epochs sealed and all leases expired. Only then does the
operator persist retirement authority, scale to zero, observe empty membership,
and restore the desired count. Each surviving ordinal gets a new Pod UID and
runtime generation on its original PVC/PV. Same-host continuity or the existing
exclusive EBS handoff proof remains mandatory. Fresh live metadata, readiness,
unchanged invocation checks, a full loss scan and ten seconds of settling precede
completion. Every PVC is retained, including the removed ordinal's disk.

Pause or a changed request after stopping has begun does not abandon recovery;
the already authorized target is restored before another action can be admitted.
Removing opt-in before capture cancels admission. An incomplete seal **never**
permits scale-to-zero or replacement startup, even with downtime enabled.

There is still no safe automatic retry for an already all-stopped **unsealed**
fleet using unchanged v0.5.0. The pinned runtime starts a recovery-only follower
listener concurrently with immediate predecessor recovery (`main.rs:3316-3360,
4265-4302`), without a fleet-wide startup barrier. Its recovery classifies an
unreachable follower whose lease expired beyond three TTLs as conclusive and may
persist bounded loss (`node_log.rs:4580-4725`). Parallel Pod creation does not
prove every retained follower is serving before the first reclaim. Closing that
gap requires a runtime recovery-only startup mode with an explicit release
barrier, or a separately qualified retained-follower service. Retained PVCs and
exit code zero cannot replace this contract.
