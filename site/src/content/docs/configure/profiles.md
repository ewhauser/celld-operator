---
title: Choose a storage profile
description: Understand celld's durability model, then configure its Kubernetes workload and disks.
---

:::caution[Local storage requires the celld fork]
If you use celld with persistent local storage (`PersistentFleet`), you must use
the [ewhauser/celld fork](https://github.com/ewhauser/celld). The operator relies
on its strict shutdown and recovery contract before deleting local disks; stock
upstream celld does not provide that contract. All runtime and recovery nodes
must use a compatible fork. The fork is also required for `Bucket` fleets. See
[compatibility](../../reference/compatibility/) for the required release and image digest.
:::

Read celld's [ownership and durability overview](https://github.com/denoland/celld/blob/main/docs/README.md#ownership-and-durability)
and [durability guarantees](https://github.com/denoland/celld/blob/main/docs/guarantees.md)
to understand how it uses object storage and peer disks, when writes are
acknowledged, and how recovery works. Those runtime tradeoffs should guide your
choice of profile.

This page covers the Kubernetes configuration for that choice. Both profiles
also require the strict launcher; upstream documentation does not replace the
operator's disk-removal contract.

For `profile: Bucket`, choose `bucketWorkload: Ordered` if you need deterministic
scale-in. Bucket uses temporary local disk and does not need CSI or PVCs.

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
