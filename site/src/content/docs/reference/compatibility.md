---
title: Compatibility
description: Required platform, strict runtime fork and immutable artifact pins.
---

All paths remain experimental. Use an explicit compatible fork image; the
operator supplies no default runtime digest.

| Component | Requirement |
| --- | --- |
| Kubernetes | 1.31 or newer, IPv4 Pod networking and enforced NetworkPolicy. |
| Runtime | `ghcr.io/ewhauser/celld@sha256:...`, strict shutdown schema 1. |
| Recovery fleet | Homogeneous compatible fork including native `bucket_complete` readers. |
| Launcher | Digest-pinned image built from the matching operator source; required for both profiles. |
| Persistent storage | Supported dynamic CSI, RWOP, `Delete`, `WaitForFirstConsumer` and external-provisioner deletion finalizer. |
| Capacity policy | Metrics Server for built-in observations; Prometheus is optional. |

The [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.3)
is based on upstream v0.5.1. Its published Linux amd64/arm64 index is:

```text
ghcr.io/ewhauser/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29
```

The source revision is `739f2baa87a5bfc4bfe04e317adf6d774edf8740`.
Verify source, platform and digest for the artifact you deploy. Stock upstream releases do not expose this strict contract. A
syntactically valid pin does not establish runtime/storage-format qualification.

Use `.3` for new fleets. `.1` can stall populated full-stop removal; `.2` fixes
that shutdown issue but can lose acknowledged writes when a retained recovery
peer starts late. `.3` keeps unreachable witnesses undecided and fails startup
with retained evidence if its bounded retries are exhausted. It cannot repair
loss already recorded by an older runtime. See [recovery troubleshooting](../../troubleshoot/recovery/).

The [.2 → .3 upgrade](../../qualification/runtime-upgrade/) and the
[.3 native and Kind qualification](../../qualification/native-peer-startup/)
passed for the recorded artifacts. EKS/EBS remains unqualified; an image pin or
schema number alone does not establish the same behavior for another build.

Runtime image changes use [coordinated maintenance](../../operate/upgrade-runtime/).
They are not rolling updates and do not provide automatic format migration or
rollback compatibility. The caller must qualify the source/target pair.

There is no migration from the former lifecycle journal, no stock-version
adapter fallback, no layout conversion and no reuse of historical retained
disks. Create fresh evaluation fleets with the current API and dedicated buckets.
See [capabilities and limits](../limitations/) for actual validation boundaries.
