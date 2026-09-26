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

A PersistentFleet member keeps its disk. The StatefulSet restarts one member at
a time, highest ordinal first, and waits for each to be Ready before the next;
see the [PersistentFleet lifecycle](../../concepts/current-operation/). The
rollout works the same way whatever fork build the fleet runs, including builds
before `.5`.

Neither profile has an old release adapter or automatic rollback. Watch the
conditions and [troubleshoot waits](../../troubleshoot/lifecycle/).
