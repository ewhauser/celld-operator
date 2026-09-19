---
title: Upgrade the celld runtime
description: Move a launcher-managed PersistentFleet from v0.4.1 to v0.5.0 with coordinated downtime.
---

The only implemented image transition is **v0.4.1 → v0.5.0** for a launcher-managed `PersistentFleet`. It stops the entire fleet, retains its disks, changes the image at zero replicas, and resumes the original count. Plan an outage. Bucket upgrades, arbitrary digests, rolling upgrades, and rollback are blocked. See [compatibility](../../reference/compatibility/) and [limitations](../../reference/limitations/).

## Check the source fleet

Confirm `my-fleet` in namespace `fleets` is running the exact v0.4.1 release digest, uses the trusted launcher, and has no active lifecycle operation or loss finding. Arrange application downtime before changing the image.

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
```

The approved v0.5.0 image is `ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8`. Review the [runtime version record](../../contracts/runtime-versions/) before using it.

## Request the transition

Preserve any other `maintenance` fields when editing. Set coordinated downtime and the target image together:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet \
  --type merge -p '{"spec":{"maintenance":{"allowCoordinatedDowntime":true},"runtimeImage":"ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8"}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -w
```

The controller captures exact launcher invocations, stops and seals each own log, waits for leases to expire, then scales the workload to zero. Only after zero membership does it change the image and resume the original count on retained PVCs. An unsealed log blocks the transition; do not force it through. The request and progress survive a controller restart.

Confirm the final Pod image, new Pod generations, original count, Ready condition, and a completed lifecycle outcome:

```bash
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status.lifecycle'
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
```

If `UnsupportedTransition`, `CoordinatedDowntimeBlocked`, or recovery blockers appear, use [lifecycle troubleshooting](../../troubleshoot/lifecycle/). Local Docker/MinIO evidence exists for this direction; live EKS, S3, and EBS qualification remains outstanding.
