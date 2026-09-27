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

The [fork release](https://github.com/ewhauser/celld/releases/tag/v0.6.0-ewhauser.2)
is based on upstream v0.6.0. Its published Linux amd64/arm64 index is:

```text
ghcr.io/ewhauser/celld@sha256:94f35ba6973942eb4aaad2830ee8ebe431e2a125940acf380e74782602980a2a
```

The source revision is `70a1e0b7fa16a3bccdc739331c9231f6ddc0a299`. Verify
source, platform and digest for the artifact you deploy. The fork never counts
an unreachable peer as holding no copy of a write, and it lets an idle leader
stop depending on a departed member; stock upstream releases do neither. A
syntactically valid pin does not establish runtime/storage-format qualification.

Use the `0.6.0-ewhauser.2` artifact above for new fleets. Among the
`0.5.1-ewhauser` builds, `.1` can stall populated full-stop removal; `.2` fixes that shutdown issue but can lose acknowledged writes when a
retained recovery peer starts late. `.3` keeps unreachable witnesses undecided
and fails startup with retained evidence if its bounded retries are exhausted.
It cannot repair loss already recorded by an older runtime. `.6` adds idle
probes, so an idle leader stops depending on a departed member. `.7` lets
members that lose their disks at the same time recover each other's sessions,
or record a bounded loss, instead of stalling. `0.6.0-ewhauser.1` carries all of
this onto upstream v0.6.0, and `0.6.0-ewhauser.2` adds OTLP metrics and
`OTEL_RESOURCE_ATTRIBUTES` support and drops the unused strict disk-removal and
`/state.node_log` APIs; it rolls from `0.6.0-ewhauser.1`. A PersistentFleet on a `0.5.1-ewhauser` build moves
to it with a [full stop](../../operate/upgrade-runtime/#full-stop-upgrade), not
a rolling update; Bucket fleets roll. See
[recovery troubleshooting](../../troubleshoot/recovery/).

The [.2 → .3 upgrade](../../qualification/runtime-upgrade/) and the
[.3 native and Kind qualification](../../qualification/native-peer-startup/)
passed for the recorded artifacts. An image pin or schema number alone does not
establish the same behavior for another build.

Runtime image changes [roll one member at a time](../../operate/upgrade-runtime/).
They do not provide automatic format migration or rollback compatibility. The
caller must qualify the source/target pair, including the mixed-version fleet.

There is no migration from the former lifecycle journal, no stock-version
adapter fallback, no layout conversion and no reuse of disks retained by the
former operator. Create fresh fleets with the current API and dedicated buckets.
See [capabilities and limits](../limitations/) for actual validation boundaries.
