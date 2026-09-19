---
title: Troubleshoot a fleet
description: Start with the visible symptom and open the guide for the failing stage.
---

Begin with the fleet condition and its reason. Use the same explicit cluster context for every check:

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status'
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
```

| What you see | Open |
| --- | --- |
| No fleet objects appear, the controller is unavailable, `NamespaceAccessDenied`, missing CRDs or RBAC | [Installation](../installation/) |
| Pods are Pending, PVCs do not bind, strict zone placement cannot be met, or capacity additions stay pending | [Scheduling and storage](../scheduling/) |
| A restart, scale-in, runtime upgrade, or deletion is blocked or stalled | [Lifecycle operations](../lifecycle/) |
| Capacity policy is waiting on observations or makes no useful addition | [Capacity policy](../../operate/capacity/) |

Read the [conditions reference](../../reference/conditions/) for the exact blocker reason. `Ready=True` only reports readiness at the current workload size. It can coexist with a blocked request. `Progressing=True` describes provisioning or an active lifecycle operation. A quiet Events list is normal because the operator emits blocker Events on changes.

Preserve recovery evidence while diagnosing: the cluster-wide storage reservation, journal archive ConfigMaps, retained PVCs/PVs, and object storage. Clearing status or deleting a Pod does not cancel journaled work. A `possibleLoss` finding must be investigated, never erased to force progress. The [limitations](../../reference/limitations/) page identifies gates that configuration cannot solve.
