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
| Runtime child stopped or waiting for a retained peer | [Runtime recovery](recovery/). |
| Scale-in, restart, upgrade or deletion blocked | [Blocked operations](lifecycle/). |
| Incomplete or ineffective capacity observations | [Capacity policy](../operate/capacity/). |

`Ready` describes availability. It does not replace a PersistentFleet
operation's strict proof requirements.

Use the [conditions reference](../reference/conditions/) to interpret the type,
reason and message together. Preserve current authority and storage whenever
identity or completion is uncertain.
