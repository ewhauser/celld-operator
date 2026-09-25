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

If a PersistentFleet `install-launcher` init container reports `no space left on device`, inspect node/Docker VM
bytes and inodes, image layers and ephemeral storage. This can fail before celld
starts and is not a recovery-protocol result. Reclaim only known disposable
artifacts; never delete fleet disks or recovery evidence to free space.

For PersistentFleet, inspect the PVC and StorageClass:

```bash
kubectl --context YOUR_CONTEXT -n fleets describe pvc PVC_NAME
kubectl --context YOUR_CONTEXT get storageclass STORAGE_CLASS_NAME -o yaml
```

The class must use supported dynamic CSI with `Delete` and `WaitForFirstConsumer`; every bound PV must have the expected claim UID, driver, handle and external-provisioner deletion finalizer. `StorageClassMissing` and `InvalidStorageClass` identify a missing or incompatible class. `StorageIdentityConflict` means a claim was missing, replaced, or could not be safely attributed. Preserve all disks and review the conflict; do not create a replacement claim with the same name. See [storage setup](../../configure/storage/).

When a capacity policy reports `PendingCapacity` or `IneffectiveCapacity`, compare Pod events, PVC status, and `status.capacity` to see whether the new replica joined and became useful. `IncompleteMetrics` asks for a complete, fresh Metrics Server and celld `/state` sample from every expected replica. A ready newcomer alone does not prove it redistributed demand. See [capacity](../../operate/capacity/).

Completion means the Pods reach runtime readiness, `replicaObservationValid` is true, and the blocker reason clears. If a Pod is running but the fleet remains blocked on recovery or stop evidence, use [lifecycle troubleshooting](../lifecycle/).
