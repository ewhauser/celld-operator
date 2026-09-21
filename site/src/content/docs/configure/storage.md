---
title: Storage
description: Configure fresh CSI disks that can be deleted after verified strict shutdown.
---

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

Install and configure a supported EBS CSI driver before creating the fleet. Its
provisioned PVs must carry the external-provisioner deletion finalizer and the
exact expected driver, claim UID and volume handle. The controller blocks
unrecognized, static, retained or ambiguously owned storage.

StatefulSet PVC retention remains `Retain` so Kubernetes cannot delete a claim
as an automatic side effect of replica reduction. The operator exclusively
initiates claim deletion after capturing strict runtime and launcher proof and
observing compute removal. The **StorageClass/PV reclaim policy is `Delete`**.

Cleanup waits for the captured claim and PV to disappear and for their
VolumeAttachments to clear. Kubernetes CSI finalizer behavior supplies the
backend-deletion guarantee; the operator does not call AWS APIs or remove
finalizers. An absent PVC alone is insufficient.

Contraction releases the old ordinal's claim name. Growth and coordinated
restart allocate fresh disks and recover through celld. Previously retained PVs
and EBS disks are never adopted or retroactively deleted.

Read [the exact disk contract](../../contracts/disposable-disks/) and
[qualification limits](../../reference/limitations/). Real EKS/EBS testing must
validate your driver and its physical deletion behavior.
