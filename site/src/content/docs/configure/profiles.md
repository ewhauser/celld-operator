---
title: Choose a storage profile
description: Select the workload and disk contract before creating a fleet.
---

Start with `profile: Bucket` and `bucketWorkload: Ordered` if you need deterministic
scale-in with temporary local disk. Bucket does not need CSI or PVCs. Both Bucket
layouts still require the strict launcher and compatible fork runtime.

Choose `profile: PersistentFleet` when the runtime should use persistent local
peer disks. Configure `storage.storageClassName` with a supported dynamic CSI
class using `Delete`, `WaitForFirstConsumer` and RWOP claims. See
[storage](../storage/) for the full contract.

| Setting | Bucket | PersistentFleet |
| --- | --- | --- |
| Local disk | Disk-backed `emptyDir`, bounded by `sizeGiB`. | One new RWOP claim per current ordinal. |
| Layout | Immutable Deployment or Ordered StatefulSet. | StatefulSet. |
| Scale-in | Ordered only. | Highest ordinal after strict proof. |
| Restart/upgrade | Whole captured working set with downtime permission. | Same, followed by old-disk cleanup and fresh claims. |

Neither profile provisions your bucket, runtime AWS identity, worker nodes or
public endpoint. Use one bucket per fleet. The reservation stays bound to the
original fleet UID after deletion.

Profile and storage changes require a new fleet and bucket. There is no automatic
conversion, disk adoption or compatibility path from the former operator.
