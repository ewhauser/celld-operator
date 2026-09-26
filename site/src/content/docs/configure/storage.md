---
title: Storage
description: Configure retained CSI disks for PersistentFleet.
---

:::caution[Local storage requires the celld fork]
If you use celld with persistent local storage (`PersistentFleet`), you must use
the [ewhauser/celld fork](https://github.com/ewhauser/celld). The operator relies
on its node-log state to pace changes and decide when a local disk may be
deleted; stock upstream celld does not provide that state. All runtime and recovery nodes
must use a compatible fork. The fork is also required for `Bucket` fleets. See
[compatibility](../../reference/compatibility/) for the required release and image digest.
:::

Bucket uses disk-backed `emptyDir`; `storage.sizeGiB` sets its size limit. It needs
no StorageClass. PersistentFleet requires dynamic CSI provisioning with
`ReadWriteOncePod` claims and a class such as:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ebs-delete
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  encrypted: "true"
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
```

Install and configure a supported EBS CSI driver before creating the fleet.
`StorageClassMissing` and `InvalidStorageClass` report a missing or incompatible
class. Each member's claim survives restart and upgrade; the replacement Pod
reattaches it.

StatefulSet PVC retention remains `Retain` so Kubernetes cannot delete a claim
as a side effect of replica reduction. The operator alone deletes claims, with
UID preconditions: a removed member's claim once no session needs it, a lost
disk's claim, a member named by `celld.eric.dev/replace-member`, and every claim
after fleet deletion removes the Pods. The **StorageClass/PV reclaim policy is
`Delete`**, so CSI deletes the backing volume. The operator does not call AWS
APIs or remove finalizers.

Growth allocates fresh claims and waits until any removed member's retained
claim is deleted; it never reuses one. Claims labeled with the fleet's UID are
reused if the StatefulSet itself is recreated. A claim from elsewhere at a
member's name reports `StorageIdentityConflict`.

Read the [retained disk contract](../../contracts/disposable-disks/). Verify that
your EBS CSI driver completes physical deletion in your cluster.
