---
title: Waiting and blocked changes
description: Find what a rollout, scale-in or deletion is waiting for without bypassing it.
---

Read the reason and message first:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet   -o json | jq '.status | {conditions, desiredReplicas, appliedReplicas, readyReplicas}'
kubectl --context YOUR_CONTEXT -n fleets get pods,pvc -o wide
kubectl --context YOUR_CONTEXT -n fleets get events --sort-by=.lastTimestamp
```

Blocking reasons for both profiles are `CapacityUncertain` (survivor metrics
missing or insufficient for an automatic contraction), `SchedulingBlocked`,
`InfrastructureBlocked` or `LifecycleBlocked` (an object lacks this fleet's UID
label or carries owner references; the operator refuses to adopt it),
`MaintenancePaused` and `UnsupportedTransition` (runtime is not a digest-pinned
fork pin). `Provisioning` while rolling out is normal.

## PersistentFleet waits

A PersistentFleet changes through its StatefulSet's rolling update. The operator
deletes no Pods to roll them and waits for nothing beyond readiness. It deletes
a Pod or a claim only to heal a member that cannot come back on its own; see
[self-healing](../../concepts/current-operation/#self-healing). Bucket fleets
report the same rollout and scale-in messages.

| Message | Meaning and next step |
| --- | --- |
| `Rolling out one member at a time; waiting for updated, ready replicas` | Kubernetes restarts one member at a time, highest ordinal first, and waits for each to be Ready. A member that stays unready holds the rollout. Inspect its Pod, events and logs; see [runtime recovery](../recovery/). |
| `waiting for the previous change to roll out before removing a member` | Scale-in removes one member per step, only after the previous change has rolled out. Template changes still apply meanwhile. |
| `Waiting for ready replicas; member my-fleet-2 is down; it is replaced on a fresh disk at TIME unless it returns` | `Provisioning`: one member is down and every other member has been ready for five minutes. If the member is not Ready by `TIME`, the operator replaces it on a fresh disk. No action is needed; fix what keeps it down before then if you want it to keep its disk. See [lost disks](../recovery/#lost-disks). |
| `Force-deleted Pod my-fleet-2: its node has not confirmed termination; the member returns on its own disk` | `LifecycleProgress`: the Pod stayed more than two minutes past its termination grace, so its node no longer answers. The StatefulSet recreates it. Ordered Bucket fleets report this too. See [lost nodes](../recovery/#lost-nodes). |
| `Replacing member my-fleet-2: its volume no longer exists; celld records a bounded loss for any session with no other copy` | `LifecycleProgress`: the member's claim is `Lost`. The operator deleted the claim and the Pod, and the member returns on a fresh disk. |
| `Replacing member my-fleet-2: down since TIME while the rest of the fleet is ready; it returns on a fresh disk` | `LifecycleProgress`: the member stayed down for the replacement delay. The operator deleted the claim and the Pod. |
| `Replacing member my-fleet-2: deleting its Pod so its old disk can be released` | `LifecycleProgress`: the member's claim is being deleted, by the operator or by hand, and the operator deletes the Pod that still holds it so the StatefulSet can recreate both. |
| `Deleting workload; members drain on SIGTERM`, then `Deleting disks` | `LifecycleProgress` during fleet deletion: the StatefulSet is removed first, then every fleet PVC. |

The force-delete and each replacement also record a Warning Event:
`MemberForceDeleted`, `MemberDiskLost` or `MemberReplaced`. The
`LifecycleProgress` message lasts until the next reconcile; the Event stays.

A rollout waits only for the member it restarted. It does not wait for another
member that is already down, so two members can be down at once; both keep
their disks. A member that cannot come back holds a rollout or a scale-in for
at most the replacement delay, 10 minutes by default, and a lost volume holds
nothing up. When two members are down at once, neither is replaced until one
returns, unless its volume is lost; bring one of them back.

`StorageIdentityConflict` means a PVC at a member's name lacks this fleet's UID
label; it is never adopted, and creation or growth stays blocked until you
resolve it. `DeletionBlocked` means the workload to delete is not owned by this
fleet.

Do not edit reservation annotations, manufacture replacement claims, remove
finalizers or force detach. A removed member's PVC is kept on purpose: growth
reattaches it. Delete a PVC by hand only to reclaim a removed member's disk
when the fleet will not grow back to that ordinal, or to replace a member's
disk at once instead of waiting for the operator (see
[lost disks](../recovery/#lost-disks)).
