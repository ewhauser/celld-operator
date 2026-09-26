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

The class must use supported CSI with `Delete` and `WaitForFirstConsumer`. `StorageClassMissing` and `InvalidStorageClass` identify a missing or incompatible class. `StorageIdentityConflict` means a claim at a member's name does not carry this fleet's UID label; review it and do not create a replacement claim with the same name. A Pod that cannot start because its claim is `Lost` or its PV is missing has a [lost disk](../recovery/#lost-disks); the operator deletes the claim and the Pod at once, and the member returns on a fresh disk. The operator does not judge why a member stays Pending or unready. When one PersistentFleet member stays down for the replacement delay, 10 minutes by default, while every other member has been ready for five minutes, it is replaced on a fresh disk the same way, unless it waits on its image or configuration or already runs on a disk created for its current Pod. A disk stranded in a zone with no eligible node is resolved this way. See [storage setup](../../configure/storage/).

When a capacity policy reports `PendingCapacity` or `IneffectiveCapacity`, compare Pod events, PVC status, and `status.capacity` to see whether the new replica joined and became useful. `IncompleteMetrics` asks for a complete, fresh Metrics Server and celld `/state` sample from every expected replica. A ready newcomer alone does not prove it redistributed demand. See [capacity](../../operate/capacity/).

Completion means the Pods reach runtime readiness, `replicaObservationValid` is true, and the blocker reason clears. If Pods are ready but the fleet keeps waiting, use [lifecycle troubleshooting](../lifecycle/).
