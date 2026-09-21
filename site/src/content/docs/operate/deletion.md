---
title: Delete a fleet
description: Stop the exact working set and finish disk cleanup before removing the fleet finalizer.
---

Deleting the CelldFleet requests a strict whole-fleet stop:

```bash
kubectl --context YOUR_CONTEXT -n fleets delete celldfleet my-fleet --wait=false
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

The finalizer stays while the executor captures celld completion and launcher
termination/exclusion proof for every target, conditionally removes compute and
finishes exact CSI disk cleanup. Missing or failed proof leaves deletion blocked.
Do not remove the fleet or storage finalizers manually.

PersistentFleet uses the [disposable-disk policy](../../contracts/disposable-disks/).
Deleting a verified claim asks CSI to delete its captured volume; completion
waits for the PV and attachments to clear. The controller does not call AWS APIs
or force detach. Bucket's temporary disk ends with its Pod.

The S3 bucket is not deleted. Its storage reservation remains permanently bound
to the original fleet UID. A fleet recreated with the same name cannot adopt that
bucket or a historical retained disk. Review [blocked operations](../../troubleshoot/lifecycle/)
if the request cannot finish.
