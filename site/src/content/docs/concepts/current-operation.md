---
title: PersistentFleet lifecycle
description: The StatefulSet restarts PersistentFleet members one at a time, celld recovers each one, and the operator replaces a member that cannot come back on its own.
---

PersistentFleet runs `CELLD_DURABILITY=fleet` on a StatefulSet. Each member
keeps its disk for the life of the fleet. The StatefulSet controller performs
every restart and scaling step, and celld recovers every member. The operator
renders and applies the objects and replaces a member that cannot come back on
its own, so the fleet heals without an administrator. It keeps no lifecycle
state of its own. Bucket fleets roll the same way through their Deployment or
StatefulSet; their disks are caches.

## What celld does

- On SIGTERM, a member hands its cells to the other members.
- On start, a member first serves its follower data to its peers. It then
  recovers its previous session and takes its lease. Several members, or all of
  them, can restart at once.
- The readiness probe is celld's health endpoint. It returns 200 only after
  that recovery. A new member also holds its first 200 until the fleet has
  absorbed the change: no other member is handing off cells, and every live
  member has memory headroom.
- Leaders stop using a departed member on their own. Until their ensembles
  re-form, usually within seconds, they acknowledge writes through the bucket.

## Changes

| Change | Behavior |
| --- | --- |
| Restart or upgrade | A new `maintenance.restartToken` or `runtimeImage` changes the Pod template. The StatefulSet uses `RollingUpdate`: Kubernetes restarts one member at a time, highest ordinal first, and waits for each to be Ready. Each member reattaches its disk. An operator release that changes the template rolls the same way. |
| Scale-in | One member per step, the highest ordinal, and only after the previous change has rolled out. The removed member keeps its PVC. `Automatic` and `External` contraction also require survivor-capacity evidence (`CapacityUncertain`). |
| Growth | Applied in one step. An ordinal that had a member before reattaches its kept PVC, and celld treats that as a restart. New ordinals get new claims. |
| Node drain | The PodDisruptionBudget allows `maxUnavailable: 1`. A member that is not Ready counts against it, so drains proceed one member at a time. |
| Pause | `maintenance.paused` stops the operator from writing workload changes. A rollout the StatefulSet controller has already started continues. |
| Deletion | The StatefulSet is deleted in the foreground and members drain on SIGTERM. The operator then deletes every fleet PVC, including those of removed members, and releases the finalizer. The bucket reservation is permanent. |

While the fleet exists, the operator deletes a Pod or a claim only to heal a
member; see [self-healing](#self-healing). Before it creates or grows the
StatefulSet, it checks each member's claim name: a claim without the fleet's
UID label reports `StorageIdentityConflict`. A disk from another fleet is never
adopted.

## Self-healing

No administrator is expected to intervene. On every reconcile the operator
reads each member's Pod and claim, takes at most one of these actions, and
records nothing:

| Condition | Action | Event |
| --- | --- | --- |
| A member Pod is still present more than two minutes after its termination grace ended, because its node no longer answers | Force-delete the Pod. The StatefulSet recreates it and the member keeps its disk. Also applies to Ordered Bucket fleets. | `MemberForceDeleted` |
| A member's claim is `Lost`, because its volume no longer exists | Delete the claim and the Pod at once. The StatefulSet recreates both. | `MemberDiskLost` |
| One member has been down for the replacement delay while every other member has been ready for five minutes | Delete the claim and the Pod. The member returns on a fresh disk. | `MemberReplaced` |

celld's lease fences a process that survives on a lost node, and a
`ReadWriteOncePod` disk attaches to one node at a time.

The replacement delay defaults to 10 minutes. Set it with the chart value
`memberReplacementDelay` or the operator flag `--member-replacement-delay`; it
must be at least one minute. The delay is patience, not the safety condition:
it outlasts the roughly six minutes Kubernetes takes to force-detach a volume
from a lost node, and typical node provisioning, so a member that can return
usually does so on its own disk. The delay covers a disk stranded in an unavailable
zone, a volume that no longer attaches, and a corrupt disk that keeps celld
from starting. While a member waits for it, the fleet reports `Provisioning`
with the time the member will be replaced. Two cases are left alone because a
new disk would not help: a member waiting on its image or configuration, and a
member already on a disk created for its current Pod. A claim that was only
deleted, while its volume still existed, is recreated by the StatefulSet as
soon as the member's next Pod is created.

Leaders stop using a departed member within seconds, and every member sweeps
dead leaders every 30 seconds. Once the rest of the fleet has been ready for
five minutes, no session depends on the down member's disk. A member on a
fresh disk answers recovery for its old disk with a conclusive "no fragment",
so celld seals any session whose only complete copy was on the old disk and records a
bounded loss. With the rest of the fleet ready, no such session remains unless
a second failure happened first.

[Lost disks](../../troubleshoot/recovery/#lost-disks) describes what you see
while this happens.

## Limits

- **Two members that cannot come back wait.** When two members are down at
  once, neither is replaced until one returns, unless its volume is lost.
  Replacing either could lose writes that only their disks hold. celld's
  guarantee covers the loss of one member.
- **Pacing is readiness.** Kubernetes does not wait for celld to finish
  recovering other sessions between restarts. With disks kept, a restart
  destroys nothing.
- **A rollout waits only for the member it restarted.** It does not wait for
  another member that is already down, so two members can be down at once.
  Both keep their disks, so this costs availability, not acknowledged writes.
  Kubernetes' `MaxUnavailableStatefulSet` feature gate would count every
  unavailable member, but it is off by default.
- **Removed members' disks cost storage.** They stay until growth reattaches
  them or the fleet is deleted. Delete one by hand only if you don't plan to
  grow back to that ordinal. A claim deleted before the leaders have re-formed
  their ensembles can still hold the only copy of recent writes.
- **A hand edit to the StatefulSet template starts rolling at once.** The
  operator restores its template on the next reconcile, and the member that
  started rolling returns to that template on its own disk.

## What the operator stores

Nothing about the lifecycle. The storage reservation's
`celld.eric.dev/current-operation` annotation holds only capacity-policy state,
and is absent unless the fleet has set `spec.capacity`. `status.lifecycle` is
always empty. Editing status authorizes nothing.

## Fleets from earlier releases

For a fleet created by an earlier release, the operator switches the
StatefulSet from `OnDelete` to `RollingUpdate` and writes the current template.
Kubernetes then replaces each earlier Pod, highest ordinal first, including
Pods held by the launcher scheduling gate. Claims keep their names and
identities. An in-flight strict operation is dropped with an
`OperationSuperseded` event. See [upgrade the operator](../../operate/upgrade-operator/).

The [lifecycle contract](../../contracts/current-operation/) and
[retained disks](../../contracts/disposable-disks/) describe the
implementation.
