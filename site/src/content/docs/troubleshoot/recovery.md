---
title: Runtime exit and recovery
description: Diagnose a stopped child or delayed retained witness without discarding recovery data or removal authority.
---

A running Pod does not imply that its celld child is running. The launcher starts
one child and deliberately does not restart it after an unsolicited exit. There
is no liveness or startup probe that resets this state; the application readiness
probe becomes unhealthy. Lease expiry during an S3 outage can therefore leave a
Pod `Running` but unready, with launcher state `ExitedUnrequested`.

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
`--previous` only reads a previous container in the same Pod. Record the fleet's
`status.reservation` and inspect that `CelldStorageReservation`, including its
current-operation annotation, without editing it. PersistentFleet investigations
also need the PVC/PV UIDs, CSI handle, Node UID and host boot identity. The
[storage contract](../../contracts/disposable-disks/) explains those bindings.

| Observation | Meaning and next step |
| --- | --- |
| S3 errors followed by runtime exit | Restore the runtime's bucket access and inspect logs. Restored access does not automatically restart the child or authorize disk deletion. |
| `.3` predecessor recovery reports an undecided witness | Check the named retained peer, stable peer DNS, network policy, scheduling and volume access. The first node may need the peer's early follower listener before either becomes ready. |
| Bounded startup retries exhausted | The child stops with recovery evidence retained. Resolve peer availability and investigate a coordinated administrative restart; do not erase data to make it healthy. |
| Strict removal is `Failed`, unknown, or its positive proof was lost | Follow [blocked operations](../lifecycle/). A new token or Pod cannot reconstruct a missing `data_safe` result. |
| Retired-disk marker, lock, host or boot mismatch | Preserve the disk and binding files. Never clear them to permit a replacement writer. |

## Retained-peer startup

The required `.3` fork treats an unreachable witness as undecided regardless of
lease age. While its bounded startup retries run, it serves retained follower
data but does not declare the predecessor lost merely because a peer has not
started. An expired lease fences a writer; it does not prove the peer's disk is
gone. Stable per-ordinal DNS and `publishNotReadyAddresses` are required for this
path. `.2` can lose acknowledged writes under ordinary startup skew; use the
[current artifact](../../reference/compatibility/).

The runtime still has its explicit-loss policy for reachable peers that
conclusively report missing or incomplete fragments when no complete witness
remains. The correction cannot repair loss already recorded by an older build
and is not a guarantee for permanently lost disks.

## Administrative recovery boundary

The [Kind fault test](../../qualification/native-peer-startup/) restores S3,
verifies that the exact failed children remain stopped without strict removal
proof, then explicitly replaces those Pods with UID preconditions and ordinary
deletion grace. PersistentFleet retains the same PVC/PV/CSI identity, Node UID
and boot; the replacement has a fresh Pod UID and runtime generation. It leaves
one witness stopped to test delayed startup, then restores it and verifies all
acknowledged values. The test never resets retirement markers or host bindings.

That is a qualified local scenario, not an automatic operator recovery service
or a general `kubectl delete pod` runbook. Before attempting an equivalent
intervention, establish that no issued removal is being bypassed, the disks have
not been retired, and the exact same-host/boot/storage constraints can be met.
Cross-host or cross-boot reuse remains blocked even if Kubernetes attaches the
volume. EKS/EBS recovery and permanent host loss remain unqualified.

Planned [restart](../../operate/restart/) and [runtime upgrade](../../operate/upgrade-runtime/)
are different: they require positive strict proof, dispose of the old disks,
and resume on fresh disks. They cannot be used to bypass a missing removal proof.
