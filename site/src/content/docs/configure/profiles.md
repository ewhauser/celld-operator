---
title: Choose a storage profile
description: Understand celld's durability model, then configure its Kubernetes workload and disks.
---

:::caution[Local storage requires the celld fork]
If you use celld with persistent local storage (`PersistentFleet`), you must use
the [ewhauser/celld fork](https://github.com/ewhauser/celld). It never counts an
unreachable peer as holding no copy of a write, and it lets an idle leader stop
depending on a departed member; stock upstream celld does neither. All runtime
and recovery nodes must use a compatible fork. The fork is also required for
`Bucket` fleets. See [compatibility](../../reference/compatibility/) for the
required release and image digest.
:::

Read celld's [ownership and durability overview](https://github.com/denoland/celld/blob/main/docs/README.md#ownership-and-durability)
and [durability guarantees](https://github.com/denoland/celld/blob/main/docs/guarantees.md)
to understand how it uses object storage and peer disks, when writes are
acknowledged, and how recovery works. Those runtime tradeoffs should guide your
choice of profile.

This page covers the Kubernetes configuration for that choice. Both profiles
run celld directly and change one member at a time.

`profile: Bucket` runs `CELLD_DURABILITY=bucket`: every acknowledged write is in
the bucket first, so the temporary local disk is a cache. Bucket does not need
CSI or PVCs. Choose
`bucketWorkload: Ordered` if you need deterministic zone assignment and
highest-ordinal scale-in.

Choose `profile: PersistentFleet` when the runtime should use persistent local
peer disks (`CELLD_DURABILITY=fleet`). Configure `storage.storageClassName` with a supported dynamic CSI
class using `Delete`, `WaitForFirstConsumer` and RWOP claims. See
[storage](../storage/) for the full contract.

| Setting | Bucket | PersistentFleet |
| --- | --- | --- |
| Local disk | Disk-backed `emptyDir`, bounded by `sizeGiB`. | One RWOP claim per ordinal, kept for the life of the fleet. |
| Layout | Immutable Deployment or Ordered StatefulSet. | StatefulSet. |
| Scale-in | One member per step; highest ordinal for Ordered. | Highest ordinal, one per step; its disk is kept and reattached if the fleet grows back. |
| Restart/upgrade | Rolling, one member at a time. | Rolling, one member at a time on the same disk, highest ordinal first; each member must be Ready before the next. |
| PodDisruptionBudget | `maxUnavailable: 1`; node drains evict one member at a time. | `maxUnavailable: 1`; a member that is not Ready counts against it. |

Neither profile provisions your bucket, runtime AWS identity, worker nodes or
ingress controller. Optional [routing](../networking/) can configure a public
HTTPRoute or Ingress to an existing edge. Use one bucket per fleet. The reservation stays bound to the
original fleet UID after deletion.

Profile and storage changes require a new fleet and bucket. There is no automatic
conversion, disk adoption or compatibility path from the former operator.
