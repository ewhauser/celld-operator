---
title: Troubleshoot
description: Start with the condition message and identify the stalled boundary.
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
| Scale-in, restart, upgrade or deletion blocked | [Current operation](lifecycle/). |
| Incomplete or ineffective capacity observations | [Capacity policy](../operate/capacity/). |

`Ready` describes availability. `ProductionQualified=False` and the lifecycle
qualification condition describe the experimental boundary. Neither is a
shortcut around an operation's strict proof requirements.

Use the [conditions reference](../reference/conditions/) to interpret the type,
reason and message together. Preserve current authority and storage whenever
identity or completion is uncertain.
