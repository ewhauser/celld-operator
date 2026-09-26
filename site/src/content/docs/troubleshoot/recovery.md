---
title: Runtime exit and recovery
description: Diagnose an unready member, a delayed retained witness, a lost node or a lost disk without discarding recovery data.
---

Both profiles run celld directly. An exited container restarts under kubelet;
a PersistentFleet member restarts on the same disk. Bucket members lose no
acknowledged write because the bucket already holds it.

## Collect evidence before replacing anything

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
kubectl --context YOUR_CONTEXT -n fleets logs POD_NAME -c celld --timestamps --tail=300
kubectl --context YOUR_CONTEXT -n fleets describe pod POD_NAME
kubectl --context YOUR_CONTEXT -n fleets get pvc -o wide
kubectl --context YOUR_CONTEXT -n fleets get service my-fleet-peers -o yaml
kubectl --context YOUR_CONTEXT -n fleets get endpointslices \
  -l kubernetes.io/service-name=my-fleet-peers -o yaml
```

Replace `POD_NAME` with the affected Pod. Save the logs before deleting a Pod;
`--previous` only reads a previous container in the same Pod. A PersistentFleet
member that stays down is replaced on a fresh disk after the
[replacement delay](#lost-disks), and its old disk is deleted; collect what you
need before then.

| Observation | Meaning and next step |
| --- | --- |
| S3 errors followed by runtime exit | Restore the runtime's bucket access. The container restarts and recovers from its disk and peers. |
| Predecessor recovery reports an undecided witness | Check the named retained peer, stable peer DNS, network policy, scheduling and volume access. The first node may need the peer's early follower listener before either becomes ready. |
| Bounded startup retries exhausted | The container exits with recovery data retained and restarts. Resolve peer availability; do not erase data to make it healthy. |
| A rollout stops at one member | The StatefulSet waits for the member it restarted to become Ready. Resolve that member's readiness; see [waiting changes](../lifecycle/#persistentfleet-waits). If it cannot come back, the operator replaces it after the replacement delay. |
| Pod stays `Terminating` on a node that is `NotReady` | The node no longer answers. The operator force-deletes the Pod; see [lost nodes](#lost-nodes). |
| Pod cannot start because its PVC is `Lost` or its PV is missing | The volume is gone. The operator replaces the member on a fresh disk at once; see [lost disks](#lost-disks). |
| Fleet reason `DiskRetired` or `LauncherBlocked` | Reported only by releases that ran the launcher. Upgrade the operator: current releases roll each launcher-supervised Pod onto a plain template on the same disk and ignore retirement markers. |

## Retained-peer startup

The fork treats an unreachable witness as undecided regardless of lease age.
While its bounded startup retries run, it serves retained follower data but does
not declare the predecessor lost merely because a peer has not started. An
expired lease fences a writer; it does not prove the peer's disk is gone. Stable
per-ordinal DNS and `publishNotReadyAddresses` are required for this path. `.2`
can lose acknowledged writes under ordinary startup skew; use the
[current artifact](../../reference/compatibility/).

## Lost nodes

When a node stops answering, Kubernetes evicts its Pods after the unreachable
toleration, five minutes by default. They stay `Terminating` because the
kubelet cannot confirm that they stopped. When a member Pod is still present
more than two minutes after its termination grace ended, the operator
force-deletes it. The fleet reports `LifecycleProgress` with
`Force-deleted Pod my-fleet-2: its node has not confirmed termination; the member returns on its own disk`,
and a Warning Event `MemberForceDeleted` records it. This applies to
PersistentFleet and Ordered Bucket fleets. The StatefulSet then recreates the
Pod.

A PersistentFleet member keeps its disk. celld's lease fences a process still
running on the lost node, and a `ReadWriteOncePod` disk attaches to one node at
a time. If the disk cannot attach to another node, the member stays down and is
replaced like a [lost disk](#lost-disks).

## Lost disks

A PersistentFleet member whose disk is gone or unusable cannot start. The fleet
keeps serving on the other members, and the operator replaces the member on a
fresh disk. No manual step is needed.

| Situation | What the operator does | Event |
| --- | --- | --- |
| The member's PVC is in phase `Lost`: the PV it was bound to no longer exists. | Deletes the claim and the Pod at once. | `MemberDiskLost` |
| The disk still exists but the member cannot come back, for example because the disk is stranded in an unavailable zone, no longer attaches, or is corrupt and keeps celld from starting. | Deletes the claim and the Pod once the member has been down for the replacement delay while every other member has been ready for five minutes. | `MemberReplaced` |

The replacement delay defaults to 10 minutes. Set it with the chart value
`memberReplacementDelay` or the operator flag `--member-replacement-delay`; it
must be at least one minute. The operator does not judge why a member is down.
Apart from the cases left alone below, any one member that stays down that long
while the rest of the fleet is ready is replaced. While it waits, the fleet
reports `Provisioning` with the time of the replacement:

```text
Waiting for ready replicas; member my-fleet-2 is down; it is replaced on a fresh disk at 2026-09-26T15:04:05Z unless it returns
```

If the member becomes Ready before then, nothing is replaced. When the operator
acts, the fleet reports `LifecycleProgress` with a message that starts
`Replacing member my-fleet-2`, and a Warning Event records the action. The
condition moves on at the next reconcile; the Event stays. List the fleet's
Warning Events with:

```bash
kubectl --context YOUR_CONTEXT -n fleets get events \
  --field-selector involvedObject.name=my-fleet,type=Warning
```

The StatefulSet creates a fresh claim for the new Pod. celld refuses answers
from the empty disk until the new member publishes its own lease. After that, it
seals any session whose only complete copy was on the old disk and records a
bounded loss in `log/<session>.e<epoch>.loss.json`. A session with another
complete copy recovers from it. With the rest of the fleet ready for five
minutes, no session depends on the old disk alone unless a second failure
happened first.

The operator leaves a member alone when a new disk would not help: while it
waits on its image or configuration (`ErrImagePull`, `ImagePullBackOff`,
`InvalidImageName`, `ErrImageNeverPull` or `CreateContainerConfigError`), and
when it already runs on a disk created for its current Pod. Fix the image,
Secret or placement instead. When two members are down at once, neither is
replaced until one returns, unless its volume is lost: replacing either could
lose writes that only their disks hold.

You do not have to wait for the delay. Deleting the member's claim and then its
Pod replaces its disk at once, but do it only when the disk cannot come back
and every other member has been Ready for at least five minutes.

If a claim was deleted while its volume still existed, the StatefulSet creates
a fresh claim as soon as it creates the member's next Pod, and celld applies
the same rule.

## Recovering a fleet

There is no rebuild operation, and none is needed. celld recovers each
PersistentFleet member from its own disk and its peers. If a fleet is unhealthy
and nothing above applies, request a [rolling restart](../../operate/restart/)
with a new `maintenance.restartToken`. Kubernetes restarts one member at a time
on its own disk.

- **Every member down at once**, because celld was killed everywhere or every
  Pod was deleted: the StatefulSet restarts them all on their own disks and the
  fleet recovers without help. No acknowledged write is lost.
- **A member whose disk is gone or unusable** is replaced by the operator; see
  [lost disks](#lost-disks).
- **A fleet stuck under v0.0.5**, with a member down and the reason
  `DiskRetired` or `LauncherBlocked`: upgrade the operator. The StatefulSet rolls
  every member onto plain celld on its own disk, and the retired member rejoins
  on that disk.

Do not edit finalizers, the storage reservation's annotations or claims by
hand, and do not delete every member's disk. A fresh disk refuses answers until
its member publishes a lease, and a member publishes its lease only after
recovering its previous session. With every disk fresh while sessions are
still open, no member can recover.
