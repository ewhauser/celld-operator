---
title: Delete a fleet
description: Remove compute, then for PersistentFleet its disks, before removing the fleet finalizer.
---

Delete the CelldFleet:

```bash
kubectl --context YOUR_CONTEXT -n fleets delete celldfleet my-fleet --wait=false
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

The operator deletes the workload in the foreground; members drain on SIGTERM.
A Bucket fleet's temporary disks end with their Pods, and the finalizer is
released once the workload is gone.

For PersistentFleet, once the StatefulSet and its Pods are gone the operator
deletes every fleet PVC, including those kept for members removed by scale-in,
then releases the finalizer. The StorageClass `Delete` reclaim policy asks CSI
to delete each volume. The controller does not call AWS APIs or force detach.
Do not remove the fleet or storage finalizers manually.

The S3 bucket is not deleted. Its storage reservation remains permanently bound
to the original fleet UID. A fleet recreated with the same name cannot adopt that
bucket. A workload without the fleet's UID label, or with an owner, reports
`DeletionBlocked`; see [lifecycle troubleshooting](../../troubleshoot/lifecycle/).
