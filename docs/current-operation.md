# PersistentFleet lifecycle

PersistentFleet runs `CELLD_DURABILITY=fleet` on a StatefulSet. Each member
keeps its disk for the life of the fleet. The StatefulSet controller performs
every restart and scaling step, and celld recovers every node. The operator
renders and applies the objects and replaces a member that cannot come back on
its own, so the fleet heals without an administrator. It stores nothing about
the lifecycle ([ADR 0024](decisions/0024-persistentfleet-is-a-statefulset.md)).

## What celld does

- On SIGTERM, a node hands its cells to the other nodes. It seals its own log
  once the bucket covers it.
- On start, a node first serves its follower data to its peers. It then
  recovers its previous session from its followers and takes its lease. Several
  nodes, or all of them, can restart at once.
- The health endpoint, which is the readiness probe, returns 200 only after
  that recovery. A new node also holds its first 200 until the fleet has
  absorbed the change: no other node is handing off cells, and every live node
  has memory headroom.
- Leaders stop using a departed member on their own. Until their ensembles
  re-form, they acknowledge writes through the bucket.

## Lifecycle

| Change | Behavior |
| --- | --- |
| Restart, upgrade | `maintenance.restartToken` (Pod template annotation `celld.eric.dev/restart-token`) or `runtimeImage` changes the template. The StatefulSet uses `RollingUpdate`: Kubernetes restarts one member at a time, highest ordinal first, and waits for each to be Ready. Disks are kept. An operator release that changes the template rolls the same way. |
| Scale-in | One member per step, the highest ordinal, and only after the previous change has rolled out. The removed member keeps its PVC. Automatic and External contraction also require survivor-capacity evidence (`CapacityUncertain`). |
| Growth | Applied one run of ordinals at a time, so kept disks are placed before fresh ones; see [where a fresh disk goes](#where-a-fresh-disk-goes). An ordinal that had a member before reattaches its kept PVC, and celld treats that as a restart. New ordinals get new claims. A claim at a member's name without the fleet's UID label, anywhere in the requested growth, reports `StorageIdentityConflict` and no member is added. |
| Node drain | The PodDisruptionBudget allows `maxUnavailable: 1`. A member that is not Ready counts against it, so drains proceed one member at a time. |
| Pause | `maintenance.paused` stops the operator from writing workload changes and from replacing members. A rollout the StatefulSet controller has already started continues. |
| Full stop | A runtime pair that cannot run together, such as `0.5.1-ewhauser` to `0.6.0-ewhauser`, is upgraded by pausing the fleet, scaling its StatefulSet to zero, then setting the new image and resuming in one change. Every member returns on its own disk at the declared count, whatever the capacity policy. See [upgrade the runtime](../site/src/content/docs/operate/upgrade-runtime.md#full-stop-upgrade). |
| Deletion | Foreground StatefulSet deletion (members drain on SIGTERM), then every fleet PVC, including those of removed members, then the finalizer. The bucket reservation is permanent. |

## Self-healing

No administrator is expected to intervene. On every reconcile the operator
reads each member's Pod and claim, takes at most one of these actions, and
records nothing:

| Condition | Action | Event |
| --- | --- | --- |
| A member Pod is still present more than two minutes after its termination grace ended, because its node no longer answers | Force-delete the Pod. The StatefulSet recreates it and the member keeps its disk. Also applies to Ordered Bucket fleets. | `MemberForceDeleted` |
| A member's claim is `Lost`, because its volume no longer exists | Delete the claim and the Pod. The StatefulSet recreates both. | `MemberDiskLost` |
| One member has been down for the replacement delay while every other member has been ready for five minutes | Delete the claim and the Pod. The member returns on a fresh disk. | `MemberReplaced` |

The replacement delay defaults to 10 minutes and is set with the operator's
`--member-replacement-delay` flag (chart value `memberReplacementDelay`). The
delay is patience, not the safety condition. It outlasts the roughly six
minutes Kubernetes takes to force-detach a volume from a lost node, and typical
node provisioning, so a member that can return usually does so on its own disk.
The delay covers a disk stranded in an unavailable zone, a volume that no
longer attaches, and a corrupt disk that keeps celld from starting. While a
member waits for it, the fleet reports `Provisioning` with the time it will be
replaced. Two cases are left alone because a new disk would not help: a member
waiting on its image or configuration, and a member already on a disk created
for its current Pod. A claim that was only deleted, while its volume still
existed, is recreated by the StatefulSet as soon as the member's next Pod is
created.

Leaders stop using a departed member within seconds, and every node sweeps
dead leaders every 30 seconds. Once the rest of the fleet has been ready for
five minutes, no session depends on the down member's disk. A member on a
fresh disk declares its old disk lost: it answers recovery for that disk with
a conclusive "no fragment", and celld seals any session whose only complete
copy was on the old disk and records a bounded loss in
`log/<session>.e<epoch>.loss.json`. With the rest of the fleet
ready, no such session remains unless a second failure happened first.

### Where a fresh disk goes

A member's disk pins it to the zone where the disk was created. The zone
spread counts only Pods that are on nodes, so a member whose Pod is pending
leaves its zone looking empty. A fresh disk scheduled at that moment can take
that zone, and the pending member is then left with no zone it may run in
([#79](https://github.com/ewhauser/celld-operator/issues/79)). The operator
therefore gives a member a fresh disk, by deleting its claim or the Pod that
still holds a deleted claim, only when both of these hold:

- The StatefulSet already runs the operator's current template and replica
  count and has observed them, so no rollout is about to recreate another
  member. A reconcile that writes the StatefulSet replaces nothing.
- The scheduler has decided every other member's Pod: the Pod is on a node, or
  the scheduler has found no node for it. A Pod that is being recreated, or
  that the scheduler has not tried yet, is waited for.

The fresh disk then goes to the zone the fleet is missing. A lost volume is
normally replaced within seconds, once a member that a rollout is restarting
has been placed. An operator upgrade from v0.0.5 no longer replaces a member
that v0.0.5 left down in the same step that starts the rollout. The rollout
restarts that member on its own disk, and it is replaced only if it still
cannot come back.

Growth follows the same rule. The StatefulSet creates every new Pod at once,
so growing in one write could schedule a member that reattaches a kept disk
alongside a member on a fresh disk. The operator therefore grows one run of
ordinals at a time: consecutive ordinals that all have a kept claim, or
consecutive ordinals that all get a new one. The run starting at the current
replica count is added first, which is the kept run whenever the fleet grows
back to removed members. Each run waits until the StatefulSet has observed its
spec and the scheduler has placed, or found no node for, every current
member's Pod. While it waits the fleet reports `Provisioning`:

```text
Waiting for the scheduler to place member my-fleet-2, or find no node for it, before adding members my-fleet-3 to my-fleet-4 on fresh disks
```

The runs are read from the claims on each reconcile, so nothing is recorded. A
manual, External or full-stop target is held in the fleet's spec or capacity
history and reached over as many runs as it takes. A capacity-policy addition
that spans a kept and a fresh ordinal is cut at the end of its first run and
recorded as the step taken: its redistribution is judged against that run, and
the policy adds more after its next stable window and cooldown if pressure
remains.

A member whose Pod the scheduler cannot place is named in the fleet's status
with the scheduler's reason. When it is the one member down, it is replaced
after the replacement delay like any other, and its fresh disk goes wherever
the zone spread allows. When the operator does not replace it, because another
member is also down or because it has no disk yet and waits only for a node,
the fleet reports `Blocked` with reason `MemberUnschedulable` once the member
has gone unscheduled for the replacement delay:

```text
Member my-fleet-2 has not been scheduled since 2026-09-26T15:04:05Z: 0/6 nodes are available: 2 node(s) didn't match pod topology spread constraints, 4 node(s) didn't match PersistentVolume's node affinity. preemption: 0/6 nodes are available: 6 Preemption is not helpful for scheduling; it is not replaced while member my-fleet-1 is also down
```

`Blocked` clears when the member is scheduled or replaced.

## Recovering a fleet

There is no rebuild operation. celld recovers every member from its own disk
and its peers, so the only lever an administrator needs is a rolling restart:
a new `maintenance.restartToken`. Kubernetes restarts one member at a time on
its own disk and waits for each to be Ready.

- **Every member down at once.** Whether celld was killed on every member or
  every Pod was deleted, the StatefulSet restarts all of them on their own
  disks. Each serves its follower data before it recovers its own session, so
  the fleet recovers without help and loses no acknowledged write.
- **A member whose disk is lost or unusable** is replaced by the operator; see
  [self-healing](#self-healing).
- **A fleet stuck under v0.0.5**, with a member down because the launcher
  retired its disk (`DiskRetired` or `LauncherBlocked`), recovers when the
  operator is upgraded. The StatefulSet rolls every member onto plain celld on
  its own disk, and the retired member rejoins on that disk.

Never edit finalizers, the reservation annotation or claims by hand, and never
delete every member's disk. Every session whose writes were only on those
disks would be recorded as lost. Runtimes before `0.5.1-ewhauser.7` refuse
answers from a fresh disk until its member publishes a lease, so on them
recovery stalls instead.

## Limits

- **Two members that cannot come back wait.** When two members are down at
  once, neither is replaced until one returns, unless its volume is lost.
  Replacing either could lose writes that only their disks hold. celld's
  guarantee covers the loss of one node. If one of them cannot be scheduled,
  the fleet reports `MemberUnschedulable` after the replacement delay.
- **A claim deleted by hand does not wait for placement.** The StatefulSet
  gives the member a fresh disk as soon as it creates the member's Pod. If
  that disk takes the zone a pending member's disk needs, that member cannot
  be scheduled and is replaced after the replacement delay, once the rest of
  the fleet is ready. Growth and the operator's own replacements wait for
  placement.
- **Growth is paced by scheduling.** Each run of new members waits until the
  scheduler has decided every current member's Pod. A member whose Pod is
  recreated again and again, or never reaches the scheduler, holds growth
  back. A member the scheduler cannot place does not.
- **Pacing is readiness.** The operator does not wait for celld to finish
  recovering other sessions between restarts. With disks kept, a restart
  destroys nothing.
- **A rollout waits only for the member it restarted.** It does not wait for
  another member that is already down, so two members can be down at once.
  Both keep their disks, so this costs availability, not acknowledged writes.
- **Removed members' disks cost storage.** They stay until growth reattaches
  them or the fleet is deleted. Delete one by hand only if you don't plan to
  grow back to that ordinal. celld stops depending on a departed member once
  its leaders re-form their ensembles. With `0.5.1-ewhauser.6` or later that
  happens within seconds of the removal, even for an idle leader.
- **A hand edit to the StatefulSet template starts rolling at once.** The
  operator restores its template on the next reconcile. The member that
  started rolling then returns to the operator's template on its own disk.

## Kubernetes objects

The operator applies the StatefulSet (template, replica count, update strategy
and claim retention), the NetworkPolicy and the PodDisruptionBudget, and
corrects drift in the fields it owns. Services are verify-only. Objects
without the fleet's UID label, or with owner references, are refused. A
missing StatefulSet is recreated on the fleet's existing claims. Claim
retention is `Retain` for both scale-in and deletion, so only fleet deletion
removes claims.

The storage reservation's `celld.eric.dev/current-operation` annotation holds
only capacity-policy state. It is absent unless the fleet has set
`spec.capacity`.

## Upgrading from earlier releases

This applies to fleets created by v0.0.5, which used the strict executor, and
by the unreleased one-disruption controller. The operator switches the
StatefulSet from `OnDelete` to `RollingUpdate` and writes the current
template. Kubernetes then replaces each earlier Pod, highest ordinal first,
including Pods still held by the launcher scheduling gate. Claims keep their
names and identities.

An in-flight strict operation is dropped with an `OperationSuperseded` event.
The reservation annotation is rewritten to capacity history alone, or removed,
which drops the claim identities, operations and disruption times earlier
releases stored there. `status.lifecycle` is left empty.

## Regression coverage

`internal/controller/persistent_test.go` covers the rendered StatefulSet, the
fixed budget, rolling restart and upgrade on retained disks with the operator
deleting no Pod or claim, scale-in that keeps disks and growth that reattaches
them, contraction waiting for a rollout, foreign claims, workload recreation,
deletion, adoption of strict fleets, and each self-healing action together
with the cases it must leave alone. They also cover a fresh disk waiting for
the StatefulSet and the scheduler, a member that cannot be scheduled, and the
`MemberUnschedulable` blocker. These are unit tests with a fake
client. The kind suites run the same behavior against the real fork under
write load, including every member killed or deleted at once and an upgrade
from v0.0.5 with a retired disk; see [qualification](qualification/README.md).
