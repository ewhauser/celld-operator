---
title: Troubleshoot
description: Start with the condition message and identify what the fleet waits for.
---

Read the fleet first:

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
```

| Symptom | Next step |
| --- | --- |
| Operator unavailable, missing CRD or namespace access | [Installation](installation/). |
| Pod Pending, claim unbound or newcomer unready | [Scheduling and storage](scheduling/). |
| Runtime not ready, waiting for a retained peer, or a lost disk | [Runtime recovery](recovery/). |
| `Rolling update waits`, `Retaining disk ... still needed by`, or a blocked deletion | [Waiting and blocked changes](lifecycle/). |
| Incomplete or ineffective capacity observations | [Capacity policy](../operate/capacity/). |

`Ready` describes availability. For PersistentFleet it does not mean the fleet
has settled since the last disruption.

Use the [conditions reference](../reference/conditions/) to interpret the type,
reason and message together. Preserve storage whenever a disk's contents are
uncertain.
