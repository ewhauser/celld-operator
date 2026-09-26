---
title: Compatibility
description: Required platform, strict runtime fork and immutable artifact pins.
---

Use an explicit compatible fork image; the operator supplies no default runtime digest.

| Component | Requirement |
| --- | --- |
| Kubernetes | 1.31 or newer, IPv4 Pod networking and enforced NetworkPolicy. |
| Runtime | Registry-pinned OCI `@sha256:...` reference to the compatible celld fork. PersistentFleet settlement, disk release and scale-in need `/state.node_log` (`0.5.1-ewhauser.5` or later). |
| Recovery fleet | Homogeneous compatible fork including native `bucket_complete` readers. |
| Persistent storage | EBS CSI (hostpath CSI in local tests), RWOP, `Delete` reclaim policy and `WaitForFirstConsumer`. |
| Capacity policy | Metrics Server for built-in observations; Prometheus is optional. |

The [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.7)
is based on upstream v0.5.1. Its published Linux amd64/arm64 index is:

```text
ghcr.io/ewhauser/celld@sha256:c6b28dd2cc7b80ac910013df06951dc1a06409cab3f98185594fe0f246d6039e
```

The source revision is `9413a0bafd596db273649ed470ca8a1527263aec`.
Verify source, platform and digest for the artifact you deploy. Stock upstream releases do not report node-log state. A
syntactically valid pin does not establish runtime/storage-format qualification.

Use `.3` for new fleets. `.1` can stall populated full-stop removal; `.2` fixes
that shutdown issue but can lose acknowledged writes when a retained recovery
peer starts late. `.3` keeps unreachable witnesses undecided and fails startup
with retained evidence if its bounded retries are exhausted. It cannot repair
loss already recorded by an older runtime. See [recovery troubleshooting](../../troubleshoot/recovery/).

The [.2 → .3 upgrade](../../qualification/runtime-upgrade/) and the
[.3 native and Kind qualification](../../qualification/native-peer-startup/)
passed for the recorded artifacts. An image pin or schema number alone does not
establish the same behavior for another build.

Runtime image changes [roll one member at a time](../../operate/upgrade-runtime/).
They do not provide automatic format migration or rollback compatibility. The
caller must qualify the source/target pair, including the mixed-version fleet.

There is no migration from the former lifecycle journal, no stock-version
adapter fallback, no layout conversion and no reuse of historical retained
disks. Create fresh fleets with the current API and dedicated buckets.
See [capabilities and limits](../limitations/) for actual validation boundaries.
