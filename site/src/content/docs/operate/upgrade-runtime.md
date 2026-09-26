---
title: Upgrade the runtime
description: Qualify the exact fork image, then roll it out one member at a time.
---

Use a compatible digest-pinned fork and qualify its recovery/storage-format
compatibility with the currently deployed version. Every possible recovery
participant must understand the fork's native `bucket_complete` proof. A
matching image-name pattern does not establish compatibility.

The [recorded `.2 → .3` upgrade](../../qualification/runtime-upgrade/) passes
for both profiles, with 12/12 acknowledged values preserved in each fleet.
That is graceful maintenance evidence, not permission to use `.2` for crash
recovery or proof that other source/target pairs are compatible. Use the
[current artifact](../../reference/compatibility/) for new fleets.

Change only the image:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"runtimeImage":"ghcr.io/ewhauser/celld@sha256:YOUR_VERIFIED_DIGEST"}}'
```

Replace `YOUR_VERIFIED_DIGEST` with the actual 64-character lowercase digest
before running the command. Members are replaced one at a time, so old and new
versions run together until the rollout completes; qualify that mix. No
downtime permission is needed.

A PersistentFleet member keeps its disk. The operator replaces a running member
only once the fleet has [settled](../../concepts/current-operation/), highest
ordinal first. Settlement needs node-log state from `0.5.1-ewhauser.5` or later;
an older fleet, such as `.4`, rolls on readiness plus a one-minute stabilization,
so upgrading `.4` to `.5` works in place.

Neither profile has an old release adapter or automatic rollback. Watch the
conditions and [troubleshoot waits](../../troubleshoot/lifecycle/).
