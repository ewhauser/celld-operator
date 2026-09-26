# 0023: Node loss is routine

Status: accepted. Supersedes [0022](0022-celld-control-plane.md).
[0024](0024-persistentfleet-is-a-statefulset.md) supersedes its PersistentFleet
profile, controller state and celld contract sections.
The celld behavior cited here was read from `ewhauser/celld` at `bb25517`
(v0.5.1-ewhauser.4); see [Investigation](#investigation).

## Context

celld is designed so that losing one node is not an incident. In `fleet`
durability, a write is not acknowledged until every member of the leader's
follower ensemble (one or two other nodes) has fsynced it. Recovery of a dead
leader needs only one complete follower copy. In `bucket` durability, nothing is
acknowledged before the object store holds it. S3 conditional writes fence cell
ownership, so two nodes never own one cell.

0022 treats every voluntary removal as a data-safety event that needs positive,
persisted proof:

- celld's strict `remove-disk` result must report `data_safe`.
- The launcher proves the child exited, reacquires the lock and writes a
  permanent restart-deny marker.
- The controller records the proof in a CAS-guarded annotation of up to 180 KiB.
- A ten-phase state machine drives each operation: `Intent` → `Requesting` →
  `ProofCaptured` → `Apply` → `Observing` → `DeleteClaims` → `Resume` →
  `Joining`, plus `Blocked`.
- PVCs are deleted only after guarded checks, and an ambiguous step blocks
  indefinitely.

This is stricter than what celld guarantees when a node is lost unexpectedly. A
kernel panic, spot reclaim or EBS failure skips the whole protocol. Data survives
anyway because celld recovers from surviving followers. The protocol adds no
safety that the crash path lacks, but it gives planned changes a way to stop
permanently. It also only works while the departing node is alive and able to
cooperate, which is exactly when it is needed least.

### Cost

The strict path accounts for roughly 1.9k lines of controller code (`lifecycle`,
`state`, `operation_storage`, `pod_validation`, `comparison`, `launcher`,
`maintenance`, `survivors`). The launcher and control-plane client add about 1.4k
more, and over 4k lines of tests cover these paths. About a third of the
repository exists to supervise planned removals.

Recent failures come from this design:

| Issue | Mechanism |
| --- | --- |
| #60 A fleet stuck on a retired disk; recovery required deleting finalizers and reservations by hand | A permanent restart-deny marker plus "missing proof blocks forever". One dead node became a dead fleet. |
| #58 An operator upgrade rejects fleets created by older operator versions | `verifyWorkload` re-renders the workload with the *current* operator and requires an exact match. Any template change in a release breaks existing fleets. |
| #57 Istio, IRSA and Datadog admission break maintenance | Pod validation checks the exact pod shape because the proof chain is bound to the pod. Ordinary admission webhooks fail that check. |
| #53 An unexpected child exit left a pod Running but unready | The launcher deliberately held the lock so that an unsolicited exit could not be counted as removal. |
| (unfiled) Node drains and EKS node-group upgrades cannot evict fleet pods | The PDB is `maxUnavailable: 0`. |

The Bucket profile runs `CELLD_DURABILITY=bucket`, so its disk is a cache (see
[findings](#bucket-durability-disks-are-a-cache)). It still runs the launcher and
the strict proof, and Deployment contraction is blocked.

Restart and upgrade replace every disk at once with fresh claims. That creates
the multi-disk hazard the strict protocol then has to guard against. celld
already supports restarting on retained disks, including all nodes at once.

## Investigation

These findings come from reading celld `bb25517`; they were not tested in a
cluster. Paths are relative to `crates/`.

### Loss of one node, then waiting for recovery, does not lose acked writes

- **What must exist before an ack.** A fleet ack requires every current follower
  to fsync the batch before replying (`logic/log_tier.rs:146-152`,
  `celld/node_log.rs:1727-1756`, `3538-3548`). The ensemble never includes the
  leader and has at most two followers (`celld/node_log.rs:5306-5319`).
- **The leader's own copy does not count.** It is fsynced locally but recovery
  never reads it, even when the leader restarts on the same disk. Recovery
  gathers from followers and from bundles already in the bucket
  (`celld/node_log.rs:4698-4801`, `4886-4984`). The recoverable copies are
  therefore the follower copies: one or two.
- **Dead leaders are recovered from followers alone.** One complete follower copy
  is enough (`celld/node_log.rs:4775-4779`). Recovery runs in two ways:
  - lazily, when a cell is activated (`celld/actor.rs:3263-3276`);
  - eagerly, through a sweep that every fleet-mode node runs every 30 s
    (`celld/node_log.rs:5885-5953`).
- **A lost follower does not strand live leaders.** Affected leaders mark their
  ensemble degraded and wait for bucket proofs on new acks. They drain until the
  bucket covers everything shipped, publish `bucket_complete` for that epoch, and
  then open a new epoch with the remaining peers (`celld/node_log.rs:5341-5415`).
- **Every loss path needs a second failure before recovery or re-replication
  completes.** Examples:
  - a second disk is lost;
  - a leader whose ensemble had one follower crashes after losing that follower;
  - the removed node held the last complete copy of a dead session that had not
    been recovered yet.

  One loss at a time, with each loss absorbed before the next, is safe.

### Restarting on a retained disk needs no handshake

- **Before a node installs its new lease,** it recovers its own previous session
  from that session's followers (`celld/node_log.rs:4149-4174`,
  `celld/main.rs:4381-4427`).
- **Replacing the lease.** A restarted node with the same `CELLD_NODE` replaces
  its lease immediately through an ETag CAS (`logic/lib.rs:2674-2685`). If a
  zombie of the old process is still running, it self-fences.
- **Follower fragments survive the restart.** Fragments the node holds for other
  leaders stay on disk and remain available to peers. While the node recovers,
  it serves only seal and tail requests, so nodes that restart together can
  recover from each other (`celld/main.rs:3386-3396`, `3440-3472`). This depends
  on the stable DNS peer address that 0022 already uses.
- **Nothing ties readiness to a node's own recovery,** but health cannot return
  200 until that recovery finishes, because the public listener starts only
  after it.

### SIGTERM already drains

SIGTERM starts the same handoff drain as `POST /shutdown`
(`celld/main.rs:4802-4806`, `4953-4955`). The drain hands off cells, releases the
drain token and seals the node's own log when the bucket covers it
(`celld/main.rs:4988-5790`). It is bounded by `CELLD_SHUTDOWN_TOTAL_MS`. The
existing lifecycle validation already makes `terminationGracePeriodSeconds`
longer than that bound. No preStop hook is needed. The drain has no externally
checkable completion: exit is 0 either way. That does not matter, because
nothing depends on it.

### Bucket durability disks are a cache

In `bucket` mode, every ack needs a bucket proof and the node never ships a log
as a leader (`celld/node_log.rs:3712-3723`, `3817-3822`). A bucket-mode node
still creates a follower store, and fleet-mode peers could recruit it
(`celld/node_log.rs:5304-5323`). In a fleet where every node runs `bucket`, no
fragment ever arrives. The operator sets durability per fleet, so Bucket fleets
are homogeneous. Pod UID node identity means no node name is ever reused.

This is inferred from code; celld's docs do not state it. The remaining
subsystems, including Queue, alarms and containers, have not been audited for
local-only state.

### There is no `settled` signal today

- **What `/state` does not expose.** `/state` includes ownership, phases, load,
  deployment and `shutdown` (`celld/actor.rs:4136-4226`, `celld/main.rs:2921-2934`).
  It exposes nothing about node logs.
- **What health covers.** The only health endpoint is the public
  `/.well-known/celld/health`. It reports draining, the one-shot first-readiness
  gate and lease freshness (`celld/main.rs:2869-2875`, `logic/lib.rs:912-946`).
  celld emits no metrics.
- **Where node-log state lives.** It is in `nodes/<node>.json` in the bucket:
  state, epoch, ensemble and `bucket_complete` (`celld/ownership_store.rs:216-232`).
  Shipper health is held in memory (`celld/node_log.rs:5248-5254`).
- **The eager sweep already lists every record** every 30 s, but it does not
  keep the result anywhere (`celld/node_log.rs:5885-5953`).

The operator has no S3 client, by design, so celld must expose this state.

### Hazard: an empty disk answering under a reused node name

`SealReq` and `TailReq` carry only the leader session and epoch, not the member
being asked (`celld/node_log.rs:976-1008`). Recovery finds a member's address
through its lease record, which holds the stable pod DNS name. Whatever process
answers at that name speaks for the member.

If a pod comes back with an **empty** disk under the same pod name, it answers
"no fragment". Recovery treats that as a *definite* answer, not an undecided one
(`celld/node_log.rs:1817-1863`). When no complete copy exists and every member
has answered definitively, recovery writes `log/<session>.e<epoch>.loss.json` and
seals (`celld/node_log.rs:4814-4847`). An empty replacement can therefore turn a
recovery that would wait into a declared loss. This only matters when the lost
disk was the last complete copy for some session. The problem is not the loss
declaration itself: if a disk is truly gone, its data is gone and recording that
is correct. The problem is a disk that still exists, for example on an
unreachable node, being answered for by an empty stranger.

Changing `CELLD_NODE` for the replacement does not help, because the stable DNS
name is shared. 0022 avoids the hazard only because strict removal proves no
obligations remain before it creates a fresh claim under the old name.

## Decision

The operator does not guarantee safety by proving each removal. celld guarantees
it by tolerating the loss of one node. The operator's only safety job is to
avoid destroying a disk that still exists while some session depends on it, and
to avoid disrupting a second node before the fleet has absorbed the first.

The operator never hangs on a disk that is gone. If a disk no longer exists, its
data is gone and nothing can bring it back. The member is replaced and celld
records a bounded loss for any session that had no other complete copy.

### Invariants

1. **One voluntary disruption at a time.** No planned action removes or restarts
   a member while another member is down or the fleet is not `settled`.
2. **No existing disk is destroyed while it carries an obligation.** The
   operator deletes a PVC only once celld reports that no session depends on
   that member. A disk that is already gone is not waited for.
3. **Exclusive bucket ownership.** `CelldStorageReservation` stays as it is.
4. **Pinned artifacts and network isolation stay as they are.**

The operator still does not read the bucket or interpret leases. celld owns
replication and fencing and reports what the operator needs.

### celld contract

celld adds a `node_log` object to `/state` on the internal listener. It is
available on every live node:

```json
"node_log": {
  "posture": "fleet",
  "session": "fleet-a-2/…",
  "own": {"state": "open", "epoch": 7, "ensemble": ["fleet-a-0", "fleet-a-1"], "bucket_complete": false},
  "shipper_healthy": true,
  "fleet": {
    "observed_ms": 1790000000000,
    "complete": true,
    "unrecovered": [{"session": "fleet-a-1/…", "state": "recovering", "expired_ms": …}],
    "obligations": {"fleet-a-1": ["fleet-a-0/…"]}
  }
}
```

- **`own` and `shipper_healthy`** come from the node's own record and its
  in-memory shipper.
- **`fleet`** is the result of the last completed dead-leader sweep. `complete`
  is false if the LIST or any GET in that pass failed.
- **`unrecovered`** lists every record that is unsealed and whose lease has
  expired, including records another node is currently recovering.
- **`obligations`** maps each member to the sessions that still need it: every
  record whose ensemble names that member, whose epoch is not `bucket_complete`,
  and which is not sealed. This is the same predicate strict removal evaluates
  (`celld/disk_removal.rs:94-143`). Here any node can evaluate it for any
  member, including one that is already dead.

The operator derives `settled` from these fields. A fleet is `settled` when all
of the following hold:

- Every expected pod is Ready. Health 200 implies the node has recovered its own
  predecessor session.
- At least one node reports `fleet.complete` with an `observed_ms` later than the
  last disruption plus one lease TTL. That margin covers a crash that has not
  expired yet.
- Every reporting node's `unrecovered` list is empty.
- Every live fleet-mode node reports `shipper_healthy`. This does not apply to a
  one-node fleet.

A member `m` is **releasable** when every reporting node's `obligations[m]` is
empty. `unknown` counts as not settled and not releasable. Waiting never affects
the running fleet, only the next voluntary step.

celld also carries the expected member in `SealReq` and `TailReq`, and stores a
random disk-incarnation ID in the follower store, published in the lease.
Recovery compares against the member's current lease. While that lease still
names the old disk, a different disk answering at the address is refused and
counts as undecided, so a stranger cannot speak for a disk that may still exist.
Once the replacement publishes its own lease, the old disk is gone by
definition. Its empty answer is then conclusive, and recovery records a bounded
loss and seals instead of waiting forever. This is intentional: celld does not
record each member's incarnation at recruitment, because that would make a lost
disk block recovery indefinitely.

### Bucket profile

- A plain Deployment: no launcher, no scheduling gates, no operation state and
  no strict shutdown. Node identity stays the pod UID.
- Scaling in either direction just sets `replicas`. Kubernetes chooses the
  victim, which is safe because the bucket holds every acked write.
- Restart and upgrade use a standard `RollingUpdate`.
- HPA and External capacity ownership can write `replicas` directly.
- The Ordered layout becomes unnecessary. Existing Ordered fleets keep working as
  a StatefulSet without the launcher.

### PersistentFleet profile

- **Workload.** A StatefulSet using `RollingUpdate`, with the operator driving
  `partition`. PVC retention stays `Retain` for both scale-down and deletion, and
  the operator deletes PVCs explicitly. Node identity stays the pod name, and the
  advertise address stays the stable peer DNS name.
- **Restart and upgrade keep disks.** Once the fleet is `settled`, the operator
  lowers the partition by one ordinal. It waits until the pod is Ready and the
  fleet is `settled` again, then continues. The restarted node recovers its own
  session using the existing boot path. There are no fresh claims and no
  coordinated-downtime permission, because only one member is down at a time.
- **Graceful stop.** SIGTERM runs celld's own drain; no preStop hook is needed.
  If the drain doesn't finish, the member is simply treated as a crashed node.
- **Scale-in** is the only routine action that destroys a disk:
  1. Wait for `settled`.
  2. Lower `replicas` by one. The StatefulSet removes the highest ordinal, `m`.
  3. Wait until the pod is gone, `m` is **releasable** and the fleet is
     `settled`.
  4. Delete `m`'s PVC with a UID precondition.
  5. Repeat for the next member.

  After a controller restart, the step is re-derived from the cluster: replica
  count, pods present, and orphaned PVCs above `replicas`. This works whether or
  not `m` drained cleanly, so a dead member can be removed the same way.
- **Scale-out** raises `replicas`. The operator first confirms that any PVC above
  the old count has been deleted. New ordinals get fresh claims.
- **Disruption budget.** The operator manages the PDB: `maxUnavailable: 1` while
  `settled`, `0` otherwise. Node drains and cluster upgrades then proceed one
  member at a time and pause during recovery.
- **Lost disks are replaced automatically** (for #60). When a member's PVC or
  its bound PV no longer exists, that disk is gone. The operator deletes the pod
  and any leftover PVC, and the StatefulSet recreates both with a fresh disk. It
  does not wait for obligations. celld recovers each dependent session from any
  other complete copy, or records a bounded loss for it. Status reports the
  sessions that depended on the lost disk, so the loss is visible, but nothing
  blocks on it.
- **Replace a member.** An annotation on the fleet,
  `celld.eric.dev/replace-member: <ordinal>`, tells the operator that an existing
  disk should be treated as gone, for example when it is corrupt or stuck in an
  unavailable zone. The operator proceeds under the one-disruption rule without
  refusing. If the member still carries obligations, status names the sessions
  that may be recorded as lost. The annotation is the administrator's decision;
  the operator never destroys an existing disk with obligations on its own.
- Retirement markers left by 0022 are ignored once the launcher is gone.
- **Fleet rebuild** replaces every disk from S3 and remains an explicit
  administrative operation. It is releasable-gated like scale-in, applied to all
  members.

### Workload ownership

The operator applies its workload, Services, NetworkPolicy and PDB with
server-side apply under one field manager. It owns only the fields it sets and
ignores fields that admission webhooks or other controllers inject. Drift in its
own fields is corrected, not blocked. A new operator release that changes the
template produces a rolling update through the same partitioned process. This
removes the exact-recorded-infrastructure comparison, shape-based pod validation
and the `InfrastructureBlocked` class of failures.

### Controller state

The controller works from desired state and observed state. The fleet's `.status`
records:

- the last observed `settled` value and the node that reported it;
- the member currently being acted on;
- the reason the controller is waiting, including the sessions that block
  release.

Everything else is derived from the cluster. The reservation spec keeps only
bucket and fleet UID ownership.

### Launcher

The launcher is removed.

| Launcher responsibility | Replacement |
| --- | --- |
| Single writer per disk | RWOP PVC and StatefulSet identity; celld's per-cell S3 fencing; same-name restart replaces the lease through a CAS and fences a zombie |
| Restart on unexpected exit | celld exits with code 3 on a self-fence (`celld/main.rs:4956-4958`); kubelet restarts it |
| Restart spacing of at least one lease TTL (celld's supervisor requirement) | celld enforces it at startup when its own lease record is recent, or the operator ships a short entrypoint delay |
| Liveness | celld's own health endpoint, with a startup probe that allows for predecessor recovery (up to 4 × 15 min) |
| Strict removal proof | The releasable check, which also works for a dead member |
| Retired-disk marker | Not needed; a lost disk is replaced like a lost node |

## Consequences

- Voluntary steps wait only for recovery that can still happen. A disk that is
  gone never blocks the fleet: its member is replaced and any loss is recorded
  and reported. Waiting states name the sessions responsible.
- Maintenance does not require coordinated downtime.
- Operator upgrades become ordinary rolling updates, and admission-injected
  sidecars and environment variables work.
- The operator no longer depends on strict `remove-disk`. It depends on the
  `node_log` block in `/state`.
- Removing a dead member no longer requires that member's cooperation.
- Two unplanned losses close together can still lose writes whose only follower
  copies were on those disks. Unplanned losses include a leader crashing while
  its ensemble has one follower, for example in a 3-node fleet during a planned
  restart. This is celld's replication factor and is unchanged by either design.
  Fleets that need more protection should run five or more nodes, where
  ensembles stay at two followers during a single restart, or use `bucket`
  durability.

## Migration

1. **Bucket profile.** Stop reconciling the current-operation annotation for
   Bucket fleets. Render the plain Deployment with server-side apply. The launcher
   disappears in one rolling update. This is safe now and needs no celld change.
2. **celld.** Add the `/state.node_log` block and member/incarnation checking on
   seal and tail, then release a fork build that includes both.
3. **PersistentFleet.** Adopt existing StatefulSets through server-side apply and
   handle the state 0022 left behind:
   - **No in-flight operation:** drop the annotation, then roll.
   - **In-flight with proofs for every target:** those members are releasable by
     construction. Delete their PVCs and let fresh members join one at a time.
   - **Blocked or partially proven:** restart each member on its retained disk
     under the new rolling process; a retirement marker is ignored. Members whose
     disks are already gone are replaced automatically.
4. **Deletion.** Remove the launcher binary and image,
   `internal/runtime/controlplane`, the strict-path controller files and their
   tests, and the current-operation and `creation-claim-uids` annotations.

## Qualification

Replace the proof-boundary regressions with behavior tests. Run them against the
real fork in Kind, under continuous write load, and check that every acked write
is readable once the fleet is `settled`:

- Kill one member in each way: pod delete, node drain, `SIGKILL`, and PVC loss.
  After PVC loss the member is replaced without intervention.
- Rolling restart and runtime upgrade, with retained disks.
- Scale 5 → 3 and 3 → 5, including a controller restart between steps.
- Lose the last complete copy of a dead session, by deleting the disks of a
  leader and its only follower. The operator must not hang: the fleet settles
  with a recorded loss for exactly that session, and every other acked write is
  readable.
- A node-group drain running in parallel with a rolling upgrade. The PDB must
  serialize them.
- Istio sidecar, IRSA and Datadog admission during all of the above.
- In celld: an empty disk under a reused name answers "undecided" while the
  member's lease still names the old disk, and "conclusive" once its own lease
  is published.
