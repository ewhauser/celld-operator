---
title: One disruption at a time
description: PersistentFleet changes one member at a time and waits until celld reports that the fleet has absorbed it.
---

celld tolerates the loss of any one member, so the operator's safety job is to
disrupt one member at a time and never delete a disk a session still needs.
Bucket fleets follow the same pacing through their workload controller; their
disks are caches.

For PersistentFleet the operator reads celld's node-log state from every member.
The fleet is **settled** when every member is Ready in `fleet` durability with a
follower ensemble, and a complete dead-leader sweep made after the last
disruption plus one lease TTL lists no unrecovered session. Unknown state is
never settled.

1. **Restart or upgrade:** the operator deletes one outdated Pod at a time,
   highest ordinal first, and waits to settle before the next. A member already
   down is replaced at once. Disks are kept.
2. **Scale-in:** when settled, the highest ordinal is removed. Its disk is
   deleted only once no fresh sweep lists a session that needs it.
3. **Lost disk:** a member whose claim is `Lost`, whose PV is gone or whose Pod
   waits for a deleted claim is replaced at once. celld recovers each session
   from another copy or records a bounded loss; status names those sessions.
4. **Node drain:** the PodDisruptionBudget allows one eviction while settled
   and none while recovering.

The reservation keeps applied count, image, restart token, last disruption and
capacity-policy state. Editing status cannot authorize anything. Runtimes before
`0.5.1-ewhauser.5` report no node-log state; they can still roll restarts on
readiness plus a one-minute stabilization but never release a disk.

Earlier releases used a bounded current operation with strict launcher proof.
An operation left in flight by them is recorded as `Superseded`. See
[the lifecycle contract](../../contracts/current-operation/) and
[retained disks](../../contracts/disposable-disks/).
