---
title: Storage
description: Prepare a dedicated S3 bucket and, for PersistentFleet, a retained EBS StorageClass.
sidebar:
  order: 3
---

Every fleet reserves an **entire S3 bucket**. The bucket name is bound permanently to that fleet UID by a cluster-scoped CelldStorageReservation. Do not reuse a bucket for another fleet or add lifecycle expiry for nodes/ and log/ recovery evidence. [AWS identities](../aws/) explains the separate writer and evidence reader roles.

Set storage.region to the AWS region that contains the bucket and the listed placement zones. storage.sizeGiB is ephemeral disk space for Bucket and the PVC request size for PersistentFleet. storage.storageClassName is required only for PersistentFleet.

## PersistentFleet StorageClass

Install the [Amazon EBS CSI driver](https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html) and grant it its own AWS permissions. Save the following as `ebs-retain-storageclass.yaml`:

~~~yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ebs-retain
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: false
~~~

The controller validates the EBS CSI provisioner, Retain and WaitForFirstConsumer. Its handoff path additionally accepts gp2 or gp3 and checks the resulting PV, PVC, CSI handle and ReadWriteOncePod access mode. WaitForFirstConsumer lets scheduling select a zone before an EBS volume is provisioned; EBS remains tied to that zone. See [Kubernetes volume binding](https://kubernetes.io/docs/concepts/storage/storage-classes/#volume-binding-mode) and [EBS CSI](https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html).

~~~sh
kubectl --context YOUR_CONTEXT apply -f ebs-retain-storageclass.yaml
kubectl --context YOUR_CONTEXT get storageclass ebs-retain -o yaml
~~~

The operator exclusively creates each ordinal PVC before the StatefulSet consumes it. It refuses to adopt a pre-existing matching claim, because its name alone does not prove disk identity. On deletion it retains the PVCs, PVs, reservation and journal; follow the [deletion procedure](../../operate/deletion/) rather than deleting them to unblock the CR. Back up reservation journal archives together with the reservation. See [lifecycle journal](../../concepts/lifecycle-journal/).

The Bucket profile uses disk-backed ephemeral storage with the requested size limit. It does not reference a StorageClass or retain per-member PVCs. Its durability still depends on the configured S3 bucket and runtime protocol.
