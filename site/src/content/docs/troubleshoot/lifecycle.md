---
title: Waiting and blocked changes
description: Find what a rollout, scale-in, replacement or deletion is waiting for without bypassing it.
---

Read the reason and message first:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet   -o json | jq '.status | {conditions, lifecycle, desiredReplicas, appliedReplicas, readyReplicas}'
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

`LifecycleProgress` means the operator is proceeding one member at a time. The
message names the next step and why it waits:

| Message | Meaning and next step |
| --- | --- |
| `Rolling update waits before POD: ...`, `Contraction waits for the fleet to settle: ...`, `Fleet is recovering: ...` | The fleet has not [settled](../../concepts/current-operation/) since the last disruption. The suffix names the first unmet condition: a member not ready, node-log state unavailable, no follower ensemble, no complete sweep yet, or an unrecovered session. Most clear within seconds of the lease TTL. |
| `member POD node-log state unavailable: runtime reports no node-log state` | The runtime predates `0.5.1-ewhauser.5`. Restarts and upgrades still roll after a one-minute stabilization; scale-in and disk release wait until every member runs `.5` or later. |
| `Retaining disk data-FLEET-N of removed member FLEET-N: still needed by ...` | celld's latest sweep lists sessions whose current epoch still needs that disk. The operator deletes it once none do. Growth waits for it too. Keep the removed member's peers healthy; do not delete the PVC by hand. |
| `... fleet node-log state unknown` | No fresh complete sweep exists, so the answer is unknown. Check member readiness and `/state` reachability on port 8081. |
| `Replacement of POD waits for the fleet to settle: ...` | A `celld.eric.dev/replace-member` request runs under the one-disruption rule. |

A session stuck as unrecovered keeps the fleet unsettled. Inspect its named
state in the runtime logs of every member; the operator does not override
celld's recovery decision.

`ReplaceMemberInvalid` means the annotation names no current member; use an
ordinal such as `2` or a Pod name such as `my-fleet-2`. `StorageIdentityConflict`
means a PVC at a member's name lacks this fleet's UID label; it is never adopted.
`DeletionBlocked` means the StatefulSet to delete is not owned by this fleet.

Do not edit reservation annotations, manufacture replacement claims, remove
finalizers or force detach. For a disk that is gone or unusable, see
[lost disks](../recovery/#lost-disks).
