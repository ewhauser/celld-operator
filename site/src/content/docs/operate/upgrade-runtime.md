---
title: Upgrade the runtime
description: Qualify the exact fork image and change it through coordinated maintenance.
---

Use a compatible digest-pinned fork and qualify its recovery/storage-format
compatibility with the currently deployed version. Every possible recovery
participant must understand the fork's strict proof. A matching image-name
pattern does not establish compatibility.

Request the new image and allow whole-fleet downtime:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"runtimeImage":"ghcr.io/ewhauser/celld@sha256:YOUR_VERIFIED_DIGEST","maintenance":{"allowCoordinatedDowntime":true}}}'
```

Replace `YOUR_VERIFIED_DIGEST` with the actual 64-character lowercase digest
before running the command. The executor strictly stops the current working set,
completes disk cleanup, then starts fresh disks with the recorded new image. It
waits for readiness before reporting completion.

This is coordinated maintenance for either profile. There is no rolling image
update, old release adapter or automatic rollback. A changed image request cannot
retarget an operation that has already reached Requesting. Watch
`status.lifecycle` and [troubleshoot blockers](../../troubleshoot/lifecycle/).
