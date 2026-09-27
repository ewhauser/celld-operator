# 0023: Node loss is routine

Status: accepted and implemented. [0024](0024-persistentfleet-is-a-statefulset.md)
records the PersistentFleet lifecycle built on it. This record replaced 0022's
strict removal proof. 0022, the first PersistentFleet lifecycle proposed here
and the migration from 0022's state are in Git history.

## Context

celld is designed so that losing one node is not an incident. In `fleet`
durability, a write is not acknowledged until every member of the leader's
follower ensemble (one or two other nodes) has fsynced it, and recovery of a
dead leader needs only one complete follower copy. In `bucket` durability,
nothing is acknowledged before the object store holds it. S3 conditional writes
fence cell ownership, so two nodes never own one cell.

0022 treated every voluntary removal as a data-safety event. A launcher
captured celld's strict `remove-disk` result, proved the child had exited and
wrote a permanent restart-deny marker. The controller then recorded that proof
in a CAS-guarded annotation and drove a ten-phase operation that blocked
indefinitely on any ambiguous step. A crash, spot reclaim or EBS failure skips
all of that, and data survives anyway because celld recovers from surviving
followers. The protocol added no safety the crash path lacked. It worked only
while the departing node could cooperate, and it gave planned changes a way to
stop permanently.

It was also about a third of the repository, and it caused the failures of the
time:

- A permanent restart-deny marker plus "missing proof blocks forever" turned
  one dead node into a dead fleet
  ([#60](https://github.com/ewhauser/celld-operator/issues/60)).
- Re-rendering the workload with the current operator and requiring an exact
  match rejected fleets created by older releases (#58).
- Pod-shape validation, bound to the proof chain, broke under Istio, IRSA and
  Datadog admission (#57).
- The launcher held the lock after an unexpected child exit, leaving a Pod
  Running but unready (#53).
- A `maxUnavailable: 0` budget kept node drains and node-group upgrades from
  evicting fleet Pods.

## What celld guarantees

These findings come from reading celld at `bb25517` (v0.5.1-ewhauser.4). Paths
are relative to `crates/`.

- **Loss of one node does not lose acknowledged writes.** A fleet ack requires
  every current follower to fsync the batch (`logic/log_tier.rs:146-152`,
  `celld/node_log.rs:1727-1756`). Recovery never reads the leader's own copy; it
  gathers from followers and from bundles already in the bucket
  (`celld/node_log.rs:4698-4801`). One complete follower copy recovers a dead
  leader (`celld/node_log.rs:4775-4779`), lazily on cell activation and eagerly
  through a sweep every node runs every 30 seconds
  (`celld/node_log.rs:5885-5953`). A leader that loses a follower acknowledges
  through the bucket until it re-forms its ensemble with the remaining peers
  (`celld/node_log.rs:5341-5415`). Losing writes takes a second failure before
  the first is absorbed.
- **Restarting on a retained disk needs no handshake.** A node recovers its own
  previous session before installing its new lease, and replaces its lease
  through an ETag CAS, so a surviving zombie self-fences
  (`celld/node_log.rs:4149-4174`, `logic/lib.rs:2674-2685`). While recovering,
  it serves its follower fragments to peers, so nodes that restart together
  recover from each other (`celld/main.rs:3386-3472`). Health returns 200 only
  after that recovery.
- **SIGTERM drains.** It starts the same handoff drain as `POST /shutdown`,
  bounded by `CELLD_SHUTDOWN_TOTAL_MS` (`celld/main.rs:4802-4806`). No preStop
  hook is needed, and nothing depends on the drain completing.
- **Bucket-durability disks are a cache.** A bucket-mode node needs a bucket
  proof for every ack and never ships a log as a leader
  (`celld/node_log.rs:3712-3723`). Durability is set per fleet, so no fragment
  ever reaches a Bucket fleet's disk.
- **A fresh disk under a reused name.** Recovery reaches a member through its
  stable DNS name, so a replacement on an empty disk answers for the old one.
  From `0.5.1-ewhauser.7`, that answer is conclusive: celld records a bounded
  loss for any session whose only complete copy was on the old disk, and seals
  it. A disk that is gone never stalls recovery.

## Decision

The operator does not prove removals. celld guarantees safety by tolerating the
loss of one node, and the operator never waits on a disk that is gone.

- **One voluntary disruption at a time.** Rolling updates restart one member,
  contraction removes one member per step after the previous change has rolled
  out, and the PodDisruptionBudget is a fixed `maxUnavailable: 1`.
- **Unchanged.** `CelldStorageReservation` keeps bucket ownership exclusive.
  Artifacts stay pinned, and network isolation stays as it was.
- **Bucket profile.** A Deployment, or an Ordered StatefulSet for deterministic
  zone assignment, running celld directly. Restart, runtime upgrade and
  operator upgrade use the workload's rolling update. Growth is one step.
  Kubernetes chooses the Deployment member to remove, which is safe because the
  bucket holds every acknowledged write. Automatic and External contraction
  require survivor-capacity evidence.
- **Workload ownership.** The operator writes the workload fields it renders
  and corrects drift in them. It does not compare Pods with the template, so
  admission-injected sidecars and environment work. A release that changes the
  template produces an ordinary rolling update.
- **The launcher is removed.**

| Launcher responsibility | Replacement |
| --- | --- |
| Single writer per disk | `ReadWriteOncePod` claims and StatefulSet identity, celld's per-cell S3 fencing, and the lease CAS that fences a zombie |
| Restart on unexpected exit | kubelet; celld exits with code 3 on a self-fence |
| Liveness | celld's health endpoint as the readiness probe |
| Strict removal proof | Not needed; a removal is a lost node |
| Restart-deny marker | Not needed; markers left on retained disks are ignored |

## Consequences

- No planned change blocks permanently, and removing a dead member does not
  need its cooperation.
- Maintenance does not require coordinated downtime. Node drains and cluster
  upgrades proceed one member at a time.
- Operator upgrades are rolling updates, and admission webhooks that mutate
  Pods work.
- Two unplanned losses close together can still lose writes whose only
  follower copies were on those disks, including a leader crashing while its
  ensemble has one follower, as in a 3-node fleet during a restart. That is
  celld's replication factor. Fleets that need more should run five or more
  nodes, where ensembles keep two followers during a restart, or use `bucket`
  durability.

## Qualification

The kind suites in [`hack/integration`](../../hack/integration/README.md) run
the real fork under continuous write load. A Bucket Deployment and an Ordered
Bucket fleet scale out and in, and every acknowledged write must read back.
0024 lists the PersistentFleet scenarios.
