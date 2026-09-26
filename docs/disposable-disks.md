# Retained persistent disks

`PersistentFleet` runs `CELLD_DURABILITY=fleet` on ReadWriteOncePod CSI claims
that survive restart, upgrade and scale-in. The required StorageClass has
`reclaimPolicy: Delete`, `volumeBindingMode: WaitForFirstConsumer`, and the
`ebs.csi.aws.com` provisioner. Local tests permit `hostpath.csi.k8s.io` with the
same contract. The bucket reservation remains permanent after fleet deletion.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ebs-delete
provisioner: ebs.csi.aws.com
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
parameters:
  type: gp3
  encrypted: "true"
```

The StatefulSet creates each member's claim from its claim template. Its **PVC
retention policy remains Retain** for both scale and workload deletion, so the
StatefulSet controller never removes a claim. The operator deletes a claim, with
a UID precondition, only to replace a member that cannot come back and, once
the fleet is deleted, every fleet claim. The StorageClass/PV **Delete reclaim
policy** then delegates the backing disk's removal to CSI.

## When a disk is deleted

A member keeps its disk until the fleet is deleted or the member is replaced
because it cannot come back. Restart and upgrade replace the Pod and reattach
the same claim. Scale-in keeps the removed member's claim, and growth back to
that ordinal reattaches it, which celld treats as a restart. Fleet deletion
removes every fleet claim, including those of removed members, after the
StatefulSet and its Pods are gone.

A removed member's disk costs storage until growth reattaches it or the fleet
is deleted. Delete one by hand only if the fleet will not grow back to that
ordinal; see the [limits](current-operation.md#limits).

The operator replaces a member's disk in two cases. A claim in phase `Lost` has
no volume behind it, so the operator deletes the claim and the Pod at once. A
member that has stayed down for the replacement delay, 10 minutes by default,
while every other member has been ready for five minutes, is treated as lost,
and the operator deletes its claim and Pod. The StatefulSet then creates a
fresh claim for the new Pod, and celld records a bounded loss for any session
whose only complete copy was on the old disk. A member waiting on its image or
configuration, or already on a disk created for its current Pod, is left alone.
When two members are down at once, neither is replaced until one returns,
unless a volume is lost. See [self-healing](current-operation.md#self-healing).

## What deletion establishes

For a conforming CSI provisioner, Kubernetes' deletion finalizer keeps a Delete
PV present until the backing storage has been deleted. See
[Kubernetes persistent-volume deletion protection](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#persistentvolume-deletion-protection-finalizer).
The controller does not call EC2, remove finalizers, force detach or delete
backend volumes directly. Verify that your EBS CSI driver completes deletion and
that acknowledged writes survive the failure modes you intend to rely on.
