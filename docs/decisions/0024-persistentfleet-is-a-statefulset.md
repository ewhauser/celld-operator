# 0024: PersistentFleet is a StatefulSet

Status: accepted. Supersedes the PersistentFleet profile, controller state and
celld contract sections of [0023](0023-node-loss-is-routine.md). The rest of
0023 stands: Bucket fleets are plain rolling workloads, the launcher is gone,
the operator owns only the fields it sets, and bucket reservations, pinned
artifacts and network isolation are unchanged.

## Context

0023 kept PersistentFleet disks across restart and upgrade, but it gave the
operator a lifecycle of its own:

- It read celld's `/state.node_log` from every member on each reconcile. It
  derived a fleet-wide `settled` value from it and gated each restart on it.
- It deleted outdated Pods itself, one at a time. It lowered the
  PodDisruptionBudget to zero while the fleet was not settled.
- It deleted a removed member's disk only when no celld session still needed
  it, and it held growth until that disk was gone.
- It inspected each member's claim and volume, replaced members whose disk it
  judged lost, and acted on a `replace-member` annotation.
- It stored the last disruption time, applied count, image, restart token and
  workload identity in an annotation on the storage reservation.

This is about 600 lines of controller code plus a settlement package and a
node-log client. It restates what celld already does.

celld's own documentation (upstream v0.5.1) describes how to operate it:

- Roll out with the orchestrator's rolling update. Stop a node with SIGTERM,
  wait for its replacement to report healthy, then move to the next node. A
  replacement holds its first healthy response until the fleet has absorbed the
  change: no active donor, and memory headroom on every live node. The docs say
  a deployer needs no separate fleet-level gate.
- celld does not scale itself. An external system starts and stops nodes, and
  celld hands their cells off.
- Local state persists across restarts. A restarting node serves its follower
  data to its peers before it recovers its own previous session, so nodes that
  restart together recover from each other's disks.
- Node failure is a normal input, not a recovery procedure.

Nothing in celld asks an orchestrator to delete a node's disk, to wait for
node-log settlement, or to replace members.

## Decision

The operator renders a StatefulSet and its prerequisites and applies them.
The StatefulSet controller performs every restart and every scaling step, and
celld recovers every node. The operator replaces a member that cannot come
back on its own, so the fleet heals without an administrator. It keeps no
lifecycle state.

- **Rollout.** `RollingUpdate` with partition 0. Kubernetes restarts one member
  at a time, highest ordinal first, and waits for each one to be Ready. The
  readiness probe is celld's health endpoint, which returns 200 only after the
  node has recovered its predecessor session and passed celld's first-readiness
  gate. Restart, runtime upgrade and operator upgrade all take this path.
- **Disks.** A member keeps its disk until the fleet is deleted or the member
  is replaced because it cannot come back (see below). Restart, upgrade and
  scale-in keep the claim. Scaling back out reattaches it, which celld treats as
  a restart on the same disk. StatefulSet claim retention stays `Retain` for
  scale-in and deletion.
- **Disruption budget.** A fixed `maxUnavailable: 1`. A Pod that is not Ready
  counts against it, so node drains proceed one member at a time.
- **Replica count.** Growth is applied in one step. Contraction removes one
  member per step, and only after the previous change has rolled out. This is
  the same rule Bucket fleets follow. Automatic and External contraction still
  require survivor-capacity evidence.
- **Storage identity.** Before creating or growing the StatefulSet, the
  operator refuses to proceed if a claim at a member's name does not carry the
  fleet's UID label. A disk from another fleet is never adopted.
- **No operator state.** The reservation annotation holds only capacity-policy
  state, which exists once a fleet sets `spec.capacity`. The operator removes
  the annotation when there is none. Fields written by earlier releases are
  dropped on the first write, including claim identities, strict operations and
  disruption times.

The operator does not read `/state.node_log` and does not release the disks of
removed members.

### Self-healing

No administrator is expected to intervene. The operator judges each member
from what the cluster reports on each reconcile, takes at most one action per
reconcile, and records nothing:

- **A Pod on a node that no longer answers.** A member Pod still present more
  than two minutes after its termination grace has ended is force-deleted, so
  the StatefulSet can recreate it. The member keeps its disk. A process that
  survives on the lost node is fenced by celld's lease, and a
  `ReadWriteOncePod` disk attaches to one node at a time. This applies to every
  StatefulSet fleet, including Ordered Bucket.
- **A lost volume.** When Kubernetes marks a member's claim `Lost`, the volume
  behind it no longer exists. The operator deletes the claim and the Pod at
  once, and the StatefulSet recreates both.
- **A member that cannot come back.** A member that has stayed down for the
  replacement delay is treated as lost if every other member has been ready for
  five minutes. The delay defaults to 10 minutes (`--member-replacement-delay`).
  It is patience, not the safety condition. It outlasts the roughly six minutes
  Kubernetes takes to force-detach a volume from a lost node, and typical node
  provisioning, so a member that can return usually does so on its own disk.
  This covers a disk stranded in an unavailable zone, a volume that no longer
  attaches, and a corrupt disk that keeps celld from starting. Leaders stop
  using a departed member within seconds, and every node sweeps dead leaders
  every 30 seconds. So once the rest of the fleet has been ready that long, no
  session depends on the down member's disk. The operator deletes the claim and
  the Pod, and the member returns on a fresh disk. Two cases are left alone
  because a new disk would not help: a member waiting on its image or
  configuration, and a member already on a disk created for its current Pod.
  Only one down member is ever replaced this way. When two are down, replacing
  either could lose writes that only their disks hold.

For a replaced disk, celld refuses answers from the empty disk until the new
member publishes its own lease. After that it records a bounded loss for any
session whose only complete copy was on the old disk, and seals it. With the
rest of the fleet ready, no such session remains unless a second failure
happened first.

## Consequences

- PersistentFleet reconciles like Bucket. `persistent.go` keeps the profile's
  storage checks, self-healing and fleet deletion. `internal/fleethealth`, the
  node-log client, the `replace-member` annotation and the settlement-driven
  budget are removed.
- The operator no longer depends on `/state.node_log`. Any runtime restarts
  under the same rolling update, including runtimes that predate node-log
  state.
- Pacing is readiness only. The operator does not wait for celld to finish
  recovering other sessions before the next restart. With disks retained, a
  restart destroys nothing. Exposure to a second, unplanned disk loss during a
  roll is celld's replication factor, as 0023 already noted.
- A rolling update waits for the member it just restarted, not for an
  unrelated member that is already down. Two members can then be down at once.
  Both keep their disks, so the cost is availability, not acknowledged writes.
  Kubernetes' `MaxUnavailableStatefulSet` gate would count every unavailable
  member, but it is off by default.
- A removed member's disk costs storage until the fleet grows back or is
  deleted. An administrator may delete such a claim by hand. celld stops
  depending on a departed member once its leaders re-form their ensembles,
  within seconds on `0.5.1-ewhauser.6` or later. A claim deleted before then can
  still hold the only copy of recent writes.
- A member that cannot come back holds up a rollout or a contraction for at
  most the replacement delay. A lost volume holds nothing up. The fleet keeps
  serving on the other members throughout.
- The operator deletes an existing disk on its own only when that disk's
  member has been down for the replacement delay while the rest of the fleet
  was ready. This is a deliberate change from 0023, where only an
  administrator could do that.
- Two members that cannot come back at the same time wait until one returns,
  unless their volumes are lost. celld's guarantee covers the loss of one node.
- The ClusterRole no longer needs PersistentVolume, Node or VolumeAttachment
  reads, and the namespaced Role no longer needs to create claims.

## Migration

Existing fleets are adopted in place. For a StatefulSet created by v0.0.5 or
by the unreleased 0023 controller, the operator changes the update strategy
from `OnDelete` to `RollingUpdate` and writes the current template. Kubernetes
then replaces each earlier Pod, highest ordinal first, including Pods still
held by the launcher scheduling gate. Retained claims keep their names and
identities. The storage reservation annotation is rewritten to the capacity
history alone, or removed.

## Qualification

The kind suites run the real fork under continuous write load and require
every acknowledged write to be readable afterwards:

- a rolling restart and a runtime upgrade on retained disks;
- a graceful Pod delete, a forced Pod delete, `SIGKILL` of celld, a node drain
  held by the budget, and an operator restart in the middle of a rollout and of
  a contraction;
- a deleted claim, which the StatefulSet replaces with a fresh disk;
- a lost volume, which the operator replaces;
- a node whose kubelet stops while celld keeps running on it. The operator
  force-deletes the stranded Pod and, after the replacement delay, replaces
  the member's disk.
- scale-in that keeps the removed member's disk, and scale-out that reattaches
  it;
- fleet deletion that removes compute, then disks, and keeps the bucket
  reservation.
