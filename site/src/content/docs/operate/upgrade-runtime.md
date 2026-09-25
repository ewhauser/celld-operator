---
title: Upgrade the runtime
description: Qualify the exact fork image, then roll it out (Bucket) or change it through coordinated maintenance (PersistentFleet).
---

Use a compatible digest-pinned fork and qualify its recovery/storage-format
compatibility with the currently deployed version. Every possible recovery
participant must understand the fork's strict proof. A matching image-name
pattern does not establish compatibility.

The [recorded `.2 → .3` upgrade](../../qualification/runtime-upgrade/) passes
for both profiles, with 12/12 acknowledged values preserved in each fleet.
That is graceful maintenance evidence, not permission to use `.2` for crash
recovery or proof that other source/target pairs are compatible. Use the
[current artifact](../../reference/compatibility/) for new fleets.

For a Bucket fleet, change only the image:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"runtimeImage":"ghcr.io/ewhauser/celld@sha256:YOUR_VERIFIED_DIGEST"}}'
```

The workload controller replaces one member at a time, so old and new versions
run together until the rollout completes; qualify that mix. No downtime
permission is needed.

For PersistentFleet, request the new image and allow whole-fleet downtime:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"runtimeImage":"ghcr.io/ewhauser/celld@sha256:YOUR_VERIFIED_DIGEST","maintenance":{"allowCoordinatedDowntime":true}}}'
```

Replace `YOUR_VERIFIED_DIGEST` with the actual 64-character lowercase digest
before running the command. The executor strictly stops the current working set,
completes disk cleanup, then starts fresh disks with the recorded new image. It
waits for readiness before reporting completion.

PersistentFleet has no rolling image update. Neither profile has an old release
adapter or automatic rollback. A changed image request cannot
retarget an operation that has already reached Requesting. Watch
`status.lifecycle` and [troubleshoot blockers](../../troubleshoot/lifecycle/).
