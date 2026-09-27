---
title: Scheduling and storage problems
description: Find why Pods cannot join, PVCs cannot bind, or capacity remains pending.
---

Start with the requested count and zone allowlist, then inspect each Pending Pod and its events. `InfrastructureReady=True` only says the generated Kubernetes objects match; it can be true while no Pod can schedule.

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
kubectl --context YOUR_CONTEXT -n fleets get pvc
kubectl --context YOUR_CONTEXT -n fleets get events --sort-by=.lastTimestamp
kubectl --context YOUR_CONTEXT get nodes -L topology.kubernetes.io/zone
```

Use `kubectl --context YOUR_CONTEXT -n fleets describe pod POD_NAME` for the affected Pod. Replace `POD_NAME` with a name from the previous command. `FailedScheduling` usually states whether the cluster lacks eligible nodes, resources, or a matching zone. The operator does not add nodes or change your explicit zone allowlist. Strict placement also keeps replicas apart on hosts; choose enough eligible nodes in the configured zones. [Placement](../../configure/placement/) explains the constraint.

For PersistentFleet, inspect the PVC and StorageClass:

```bash
kubectl --context YOUR_CONTEXT -n fleets describe pvc PVC_NAME
kubectl --context YOUR_CONTEXT get storageclass STORAGE_CLASS_NAME -o yaml
```

The class must use supported CSI with `Delete` and `WaitForFirstConsumer`. `StorageClassMissing` and `InvalidStorageClass` identify a missing or incompatible class. `StorageIdentityConflict` means a claim at a member's name does not carry this fleet's UID label; review it and do not create a replacement claim with the same name. A Pod that cannot start because its claim is `Lost` or its PV is missing has a [lost disk](../recovery/#lost-disks); the operator deletes the claim and the Pod, and the member returns on a fresh disk. The operator does not judge why a member stays Pending or unready. When one PersistentFleet member stays down for the replacement delay, 10 minutes by default, while every other member has been ready for five minutes, it is replaced on a fresh disk the same way, unless it waits on its image or configuration or already runs on a disk created for its current Pod. A disk stranded in a zone with no eligible node, or pinned to a zone the spread no longer allows, is resolved this way; see [a member that cannot be scheduled](#a-member-that-cannot-be-scheduled) and [storage setup](../../configure/storage/).

## A member that cannot be scheduled

A PersistentFleet member's disk pins it to the zone where the disk was created.
Strict placement also requires the zones to stay balanced (`maxSkew` 1). A
member whose disk is in a zone that already holds its share of members fits
nowhere, and its Pod reports `FailedScheduling` with both reasons:

```text
0/6 nodes are available: 2 node(s) didn't match pod topology spread constraints, 4 node(s) didn't match PersistentVolume's node affinity.
```

The fleet names such a member, with the scheduler's reason, in its
`Provisioning` message:

```text
Waiting for ready replicas; member my-fleet-2 cannot be scheduled (0/6 nodes are available: ...); it is replaced on a fresh disk at 2026-09-26T15:14:05Z unless it returns
```

When it is the one member down and the rest of the fleet has been ready for
five minutes, the operator replaces it after the replacement delay, 10 minutes
by default. Its new disk is created where its Pod is scheduled, in the zone
the fleet is missing. No manual step is needed.

The operator avoids putting a member in this position: it gives a fresh disk
only after the scheduler has decided every other member's Pod. Growth to a new
ordinal, or a claim deleted by hand while another member was restarting, can
still take a pending member's zone; the operator then heals it as above.

If the operator does not replace the member, the fleet reports `Blocked` with
reason `MemberUnschedulable` once the member has gone unscheduled for the
replacement delay:

```text
Member my-fleet-2 has not been scheduled since 2026-09-26T15:04:05Z: 0/6 nodes are available: ...; it is not replaced while member my-fleet-1 is also down
```

| The message says | Meaning and next step |
| --- | --- |
| `it is not replaced while member NAME is also down` | Two members are down, and replacing either could lose writes that only their disks hold. When the other member returns and the rest of the fleet has been ready for five minutes, the unschedulable member is replaced. Two members that cannot come back at once are the [limit](../../concepts/current-operation/#limits) of celld's one-member guarantee; bring the other member back. |
| No other member named | Either the rest of the fleet has not been ready for five minutes, and the member is replaced once it has, or a fresh disk would not help because the member has no disk yet or its disk was made for its current Pod. In the second case the cluster lacks a node the member fits; check the zone's nodes, taints, capacity and node autoscaler. |

`Blocked` clears as soon as the member is scheduled or replaced.

When a capacity policy reports `PendingCapacity` or `IneffectiveCapacity`, compare Pod events, PVC status, and `status.capacity` to see whether the new replica joined and became useful. `IncompleteMetrics` asks for a complete, fresh Metrics Server and celld `/state` sample from every expected replica. A ready newcomer alone does not prove it redistributed demand. See [capacity](../../operate/capacity/).

Completion means the Pods reach runtime readiness, `replicaObservationValid` is true, and the blocker reason clears. If Pods are ready but the fleet keeps waiting, use [lifecycle troubleshooting](../lifecycle/).
