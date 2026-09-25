---
title: Runtime exit and recovery
description: Diagnose an unready member, a delayed retained witness or a lost disk without discarding recovery data.
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
`--previous` only reads a previous container in the same Pod.

| Observation | Meaning and next step |
| --- | --- |
| S3 errors followed by runtime exit | Restore the runtime's bucket access. The container restarts and recovers from its disk and peers. |
| Predecessor recovery reports an undecided witness | Check the named retained peer, stable peer DNS, network policy, scheduling and volume access. The first node may need the peer's early follower listener before either becomes ready. |
| Bounded startup retries exhausted | The container exits with recovery data retained and restarts. Resolve peer availability; do not erase data to make it healthy. |
| Fleet stays unsettled on an unrecovered session | See [waiting changes](../lifecycle/#persistentfleet-waits). |
| Fleet reason `DiskRetired` or `LauncherBlocked` | Reported only by releases that ran the launcher. Upgrade the operator: current releases release the launcher gate, ignore retirement markers and roll the member onto a plain template on the same disk. |

## Retained-peer startup

The fork treats an unreachable witness as undecided regardless of lease age.
While its bounded startup retries run, it serves retained follower data but does
not declare the predecessor lost merely because a peer has not started. An
expired lease fences a writer; it does not prove the peer's disk is gone. Stable
per-ordinal DNS and `publishNotReadyAddresses` are required for this path. `.2`
can lose acknowledged writes under ordinary startup skew; use the
[current artifact](../../reference/compatibility/).

## Lost disks

A PersistentFleet member's disk is **lost** when its PVC is in phase `Lost`, the
PV it was bound to no longer exists, or its Pod has stayed Pending for a minute
with no PVC. The operator replaces that member's Pod and PVC at once, without
waiting for the fleet to settle, and emits a `MemberDiskLost` Warning Event. The
message lists sessions that needed the disk:

```text
Replacing member my-fleet-2: its disk no longer exists; celld records a loss for any of these sessions without another copy: ...
```

celld recovers each listed session from another complete follower copy, or
seals it with a bounded loss record if none remains. A fleet never waits on a
disk that is gone.

For a disk that still exists but is unusable (corrupt, or stuck in an
unavailable zone), declare it gone yourself:

```bash
kubectl --context YOUR_CONTEXT -n fleets annotate celldfleet my-fleet \
  celld.eric.dev/replace-member=2
```

The value is an ordinal or Pod name. The operator replaces that member's Pod and
PVC when the fleet is settled, or at once if that member is the only one down,
then removes the annotation and emits `MemberReplaced`. The operator never
deletes an existing disk with outstanding obligations on its own; this
annotation is the explicit request, and its sessions follow the same recovery or
loss rule. Use it only when the disk cannot come back.
