---
title: Delete a fleet and retain data
description: Request RetainData deletion and understand what remains after the CelldFleet is gone.
---

Deleting a `CelldFleet` requests a coordinated shutdown. It does **not** delete its S3 bucket, PVCs or PVs, permanent storage reservation, recovery journal, credentials, or network isolation. Plan how those retained assets will be protected before starting. The reservation cannot be assigned to a new fleet identity.

## Request deletion

First inspect the current operation and back up the fleet specification and recovery assets described in [operator upgrade](../upgrade-operator/). Then request deletion without waiting for the finalizer at the command line:

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets delete celldfleet my-fleet --wait=false
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -w
```

`Deleting=True` means the finalizer is holding the object while the operator checks exact writers and recovery evidence. Bucket deletion requires admitted and historical sessions accounted for, empty membership, expired leases, and a loss scan. PersistentFleet deletion requires authenticated launcher stop receipts, sealed own logs for captured leaders, and expired leases. The controller removes compute only after these checks. An all-stopped fleet with an unsealed log can remain blocked.

When the `CelldFleet` disappears, check retained resources and the reservation:

```bash
kubectl --context YOUR_CONTEXT -n fleets get pvc
kubectl --context YOUR_CONTEXT get celldstoragereservations
```

The absence of the fleet object confirms finalizer completion, not destruction of retained data. If deletion remains pending, inspect its `status.lifecycle` and Events, then follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/). Do not remove the finalizer, force delete Pods or detach volumes to bypass the evidence checks. Administrative disposal of retained data needs a separate, evidence-backed procedure; this operator does not provide one.
