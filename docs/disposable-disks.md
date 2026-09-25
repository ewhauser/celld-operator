# Retained persistent disks

`PersistentFleet` runs `CELLD_DURABILITY=fleet` on ReadWriteOncePod CSI claims
that survive restart and upgrade. The required StorageClass has
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

The StatefulSet's **PVC retention policy remains Retain** for both scale and
workload deletion, so the StatefulSet controller never removes a claim. The
operator alone deletes claims, with UID preconditions. The StorageClass/PV
**Delete reclaim policy** then delegates the backing disk's removal to CSI.

## When a disk is deleted

A member's disk is kept while the member exists. Restart and upgrade replace
the Pod and reattach the same claim. The operator deletes a claim only when:

- **Scale-in:** the member was removed and its disk is releasable, meaning a
  complete celld sweep observed after the last disruption lists no session that
  still needs it. Until then the fleet reports `LifecycleProgress` with
  `Retaining disk data-FLEET-N of removed member FLEET-N: still needed by ...`.
  Runtimes without node-log state (before `0.5.1-ewhauser.5`) never release one.
- **Lost disk:** the claim is `Lost`, its bound PV no longer exists, or the
  member's Pod is Pending with no claim. The Pod and claim are replaced at once.
- **Administrator request:** the `celld.eric.dev/replace-member` annotation
  names the member. The operator replaces it under the one-disruption rule.
- **Fleet deletion:** after the StatefulSet and its Pods are gone.

For a lost or replaced disk, the status message and a Warning event name the
sessions that needed it. celld recovers each from another complete copy or seals
it with a bounded loss record, so the fleet does not wait for a disk that is
gone. Growth waits until every retained disk of a removed member has been
deleted and never reuses one. See [one disruption at a time](current-operation.md).

## What deletion establishes

For a conforming CSI provisioner, Kubernetes' deletion finalizer keeps a Delete
PV present until the backing storage has been deleted. See
[Kubernetes persistent-volume deletion protection](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#persistentvolume-deletion-protection-finalizer).
The controller does not call EC2, remove finalizers, force detach or delete
backend volumes directly. Verify that your EBS CSI driver completes deletion and
that acknowledged writes survive the failure modes you intend to rely on.
