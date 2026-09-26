---
title: How removal is checked
description: celld tolerates one lost member; Kubernetes changes one member at a time and every PersistentFleet member keeps its disk.
---

celld tolerates the loss of any one member. A Bucket fleet acknowledges no write
before S3 holds it. A PersistentFleet acknowledges a write only after every
follower in the leader's ensemble has fsynced it, and recovers a dead leader
from one complete follower copy. The operator's job is to keep voluntary
disruption to one member at a time, to keep every member's disk, and to replace
a member that cannot come back on its own.

| Check | PersistentFleet rule |
| --- | --- |
| Restart and upgrade | The StatefulSet restarts one member at a time, highest ordinal first, and waits for it to be Ready. celld reports Ready only after the member has recovered its previous session. |
| Removal | One member per step, the highest ordinal, only after the previous change has rolled out. `Automatic` and `External` contraction also need survivor-capacity evidence. |
| Disk | Every member keeps its disk for the life of the fleet, including after scale-in. The operator replaces a disk only when it cannot come back; every other claim is deleted only when the fleet is deleted, after its StatefulSet is gone. |
| Node drain | The PodDisruptionBudget allows one unavailable member, and a member that is not Ready counts against it. |
| Lost node | A member Pod still present more than two minutes after its termination grace ended is force-deleted, and the member keeps its disk. celld's lease fences a process left on that node, and a `ReadWriteOncePod` disk attaches to one node at a time. |
| Foreign disk | A claim at a member's name without this fleet's UID label is never adopted. |

Readiness is the only pacing signal. The operator does not wait for celld to
finish recovering other sessions, and a rollout does not wait for a member that
was already down, so two members can be down at once. Both keep their disks,
and neither is replaced while the other is down unless its volume is lost, so
this costs availability, not acknowledged writes. Exposure to a second,
unplanned disk loss during a rollout is set by celld's replication factor, as
for any other failure.

The operator deletes an existing disk on its own only when the disk cannot come
back. A claim that Kubernetes marks `Lost` has no volume behind it and is
replaced at once. Otherwise one member must have been down for the replacement
delay, 10 minutes by default, while every other member has been ready for five
minutes. Leaders stop using a departed member within seconds, and every member
sweeps dead leaders every 30 seconds, so by then no session depends on the down
member's disk. celld refuses answers from the fresh disk until its member
publishes a lease, then records a bounded loss for any session whose only
complete copy was on the old disk. With the rest of the fleet ready, none
remains unless a second failure happened first. See
[self-healing](../current-operation/#self-healing) and
[lost disks](../../troubleshoot/recovery/#lost-disks). No EC2 fence,
force-detach or storage-finalizer removal exists.

Read the [PersistentFleet lifecycle](../current-operation/),
[retained disks](../../contracts/disposable-disks/) and
[operational limits](../../reference/limitations/).
