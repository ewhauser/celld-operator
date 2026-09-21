---
title: Choose a storage profile
description: Understand celld's durability model, then configure its Kubernetes workload and disks.
---

Read celld's [ownership and durability overview](https://github.com/denoland/celld/blob/main/docs/README.md#ownership-and-durability)
and [durability guarantees](https://github.com/denoland/celld/blob/main/docs/guarantees.md)
to understand how it uses object storage and peer disks, when writes are
acknowledged, and how recovery works. Those runtime tradeoffs should guide your
choice of profile.

This page covers the Kubernetes configuration for that choice. Both profiles
require the operator's [compatible fork runtime](../../reference/compatibility/)
and strict launcher; upstream documentation does not replace the operator's
disk-removal contract.

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
