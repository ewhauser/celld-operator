---
title: Storage
description: Configure retained CSI disks for PersistentFleet.
---

:::caution[Local storage requires the celld fork]
If you use celld with persistent local storage (`PersistentFleet`), you must use
the [ewhauser/celld fork](https://github.com/ewhauser/celld). It keeps an empty
replacement disk from answering for a member's previous disk, and it lets an
idle leader stop depending on a departed member; stock upstream celld does
neither. All runtime and recovery nodes must use a compatible fork. The fork is
also required for `Bucket` fleets. See
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
class. Each member keeps its claim for the life of the fleet: restart, upgrade
and scale-in keep it, and the replacement Pod reattaches it.

StatefulSet PVC retention is `Retain` for both scale-in and deletion, so
Kubernetes never deletes a claim as a side effect. The operator deletes a
claim, each time with a UID precondition, in two cases: to replace a member
that cannot come back (see below), and when the fleet is deleted. Once a
deleted fleet's StatefulSet and Pods are gone, it deletes every fleet claim,
including those of removed members. The **StorageClass/PV reclaim policy is
`Delete`**, so CSI deletes the backing volume. The operator does not call AWS
APIs or remove finalizers.

Growth back to a removed member's ordinal reattaches its kept claim, which
celld treats as a restart on the same disk; new ordinals get new claims. A
removed member's claim costs storage until then. Delete it by hand only if the
fleet will not grow back to that ordinal, and not before its leaders have
re-formed their ensembles, usually within seconds of the removal. Claims labeled
with the fleet's UID are also reused if the StatefulSet itself is recreated. A
claim from elsewhere at a member's name reports `StorageIdentityConflict` and is
never adopted.

A member whose volume is gone cannot start. The operator deletes its claim and
Pod at once; the StatefulSet recreates both, and the member returns on a fresh
disk. A disk that still exists but cannot come back, for example one stranded
in an unavailable zone or one that no longer attaches, is replaced the same way
once its member has been down for the replacement delay while every other
member has been ready for five minutes. The delay defaults to 10 minutes; set
it with the chart value `memberReplacementDelay`. See
[lost disks](../../troubleshoot/recovery/#lost-disks).

Read the [retained disk contract](../../contracts/disposable-disks/). Verify that
your EBS CSI driver completes physical deletion in your cluster.
