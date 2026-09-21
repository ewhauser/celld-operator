---
title: Blocked operations
description: Find the missing proof or infrastructure observation without bypassing authority.
---

Inspect the requested operation and its target:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet   -o json | jq '.status | {conditions, lifecycle, desiredReplicas, appliedReplicas, readyReplicas}'
kubectl --context YOUR_CONTEXT -n fleets get pods,pvc -o wide
kubectl --context YOUR_CONTEXT -n fleets get events --sort-by=.lastTimestamp
```

| Boundary | Check |
| --- | --- |
| Intent | Current desired request, pause, placement, runtime capability and exact target identity. |
| Requesting | Launcher reachability and exact operation/generation. HTTP acceptance is not completion. |
| Proof capture | celld data-safe result plus exact child exit, lock release and restart denial. |
| Workload effect | Original workload UID, replica count and effect marker; unexpected manual edits block progress. |
| Disk cleanup | Exact PVC/PV/driver/handle, PVC protection, CSI deletion finalizer and attachments. |
| Joining | Fresh claims, scheduling gates, Pod events and readiness. |

A timed-out Requesting operation may have reached the runtime. The controller
observes it without granting a fresh deadline; it cannot safely forget it. A
launcher crash before persisted proof can require manual recovery investigation.
Missing Pods and exit codes cannot repair lost proof.

Paused or changed requests cancel only before issuance. After issuance, finish
or diagnose the recorded operation before expecting another token, image or
replica target to apply. Bucket Deployment contraction is unsupported; use
Ordered for a new fleet when deterministic removal is needed.

Preserve current authority and affected storage while investigating. Do not edit
reservation annotations, manufacture replacement claims, remove finalizers or
force detach. See [the safety model](../../concepts/safety-model/) and
[the exact storage contract](../../contracts/disposable-disks/).
