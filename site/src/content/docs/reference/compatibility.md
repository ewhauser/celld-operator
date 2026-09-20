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

The [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.1)
is based on upstream v0.5.1. Its published Linux amd64/arm64 index is:

```text
ghcr.io/ewhauser/celld@sha256:78f74de9b5482a428b69f175cd1901b59cc363f3aa398ffd197ade0c9a6a20af
```

The source revision is `f3b7e8c07e6fee53f1752bfb7a30fffbf1d514c8`.
Verify source, platform and digest for the artifact you deploy. Stock upstream releases do not expose this strict contract. A
syntactically valid pin does not establish runtime/storage-format qualification.

Runtime image changes use [coordinated maintenance](../../operate/upgrade-runtime/).
They are not rolling updates and do not provide automatic format migration or
rollback compatibility. The caller must qualify the source/target pair.

There is no migration from the former lifecycle journal, no stock-version
adapter fallback, no layout conversion and no reuse of historical retained
disks. Create fresh evaluation fleets with the current API and dedicated buckets.
See [capabilities and limits](../limitations/) for actual validation boundaries.
