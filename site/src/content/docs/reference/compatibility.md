---
title: Compatibility
description: Required platform, strict runtime fork and immutable artifact pins.
---

Use an explicit compatible fork image; the operator supplies no default runtime digest.

| Component | Requirement |
| --- | --- |
| Kubernetes | 1.31 or newer, IPv4 Pod networking and enforced NetworkPolicy. |
| Runtime | Registry-pinned OCI `@sha256:...` reference to the compatible celld fork. Restart, upgrade and scaling work the same way on every fork build, including builds before `0.5.1-ewhauser.5` that expose no `/state.node_log`. |
| Recovery fleet | Homogeneous compatible fork including native `bucket_complete` readers. |
| Persistent storage | EBS CSI (hostpath CSI in local tests), RWOP, `Delete` reclaim policy and `WaitForFirstConsumer`. |
| Capacity policy | Metrics Server for built-in observations; Prometheus is optional. |

The [fork release](https://github.com/ewhauser/celld/releases/tag/v0.6.1-ewhauser.1)
is based on upstream v0.6.1. Its published Linux amd64/arm64 index is:

```text
ghcr.io/ewhauser/celld@sha256:e8dc139226269acf137b54b67cea42a3b6a7c6527ce968caa5d303c21c3b7b74
```

The source revision is `350202332f861e52b4f88b67670d1c0862d1cf59`. Verify
source, platform and digest for the artifact you deploy. The fork never counts
an unreachable peer as holding no copy of a write, and it lets an idle leader
stop depending on a departed member; stock upstream releases do neither. A
syntactically valid pin does not establish runtime/storage-format qualification.

Use the `0.6.1-ewhauser.1` artifact above for new fleets. Older fork builds
have these known issues and fixes:

| Build | Behavior |
| --- | --- |
| `0.5.1-ewhauser.1` | Can stall populated full-stop removal. |
| `0.5.1-ewhauser.2` | Fixes that shutdown stall, but can lose acknowledged writes when a retained recovery peer starts late. |
| `0.5.1-ewhauser.3` | Keeps unreachable witnesses undecided and fails startup with retained evidence if its bounded retries are exhausted. It cannot repair loss already recorded by an older runtime. |
| `0.5.1-ewhauser.6` | Adds idle probes, so an idle leader stops depending on a departed member. |
| `0.5.1-ewhauser.7` | Lets members that lose their disks at the same time recover each other's sessions, or record a bounded loss, instead of stalling. |
| `0.6.0-ewhauser.1` | Carries every `0.5.1-ewhauser.7` fix onto upstream v0.6.0. |
| `0.6.0-ewhauser.2` | Adds OTLP metrics and `OTEL_RESOURCE_ATTRIBUTES` support and drops the unused strict disk-removal and `/state.node_log` APIs. |
| `0.6.1-ewhauser.1` | Moves to upstream v0.6.1 and adds change export (bucket sink in release images) and the optional DynamoDB control plane. |

A fleet on `0.6.0-ewhauser.1` rolls to `0.6.0-ewhauser.2`, which rolls to
`0.6.1-ewhauser.1`. A PersistentFleet
on any `0.5.1-ewhauser` build moves to a `0.6.x-ewhauser` build with a
[full stop](../../operate/upgrade-runtime/#full-stop-upgrade), not a rolling
update; Bucket fleets roll. See
[recovery troubleshooting](../../troubleshoot/recovery/).

The recorded
[`0.5.1-ewhauser.2` → `0.5.1-ewhauser.3` upgrade](../../qualification/runtime-upgrade/)
and the
[`0.5.1-ewhauser.3` native and Kind qualification](../../qualification/native-peer-startup/)
passed for those artifacts. An image pin or schema number alone does not
establish the same behavior for another build.

Runtime image changes [roll one member at a time](../../operate/upgrade-runtime/).
They do not provide automatic format migration or rollback compatibility. The
caller must qualify the source/target pair, including the mixed-version fleet.

There is no migration from the former lifecycle journal, no stock-version
adapter fallback, no layout conversion and no reuse of disks retained by the
former operator. Create fresh fleets with the current API and dedicated buckets.
See [capabilities and limits](../limitations/) for actual validation boundaries.
