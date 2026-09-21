---
title: Monitor a fleet
description: Read conditions, operation progress, Events, and optional metrics.
---

Start with the `CelldFleet`; its conditions explain whether the fleet is ready, blocked, paused, or deleting. Always specify the cluster context and namespace.

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status'
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
kubectl --context YOUR_CONTEXT -n fleets get events --sort-by=.lastTimestamp
```

Compare `desiredReplicas`, `appliedReplicas`, `observedReplicas`, and `readyReplicas` to find the stage that stopped. `replicaObservationValid=false` means the Pod list was incomplete; do not treat a zero count as proof that all processes stopped. `blockedSince` measures how long the current blocker has persisted. `status.lifecycle` gives the current operation phase, fixed deadline and blocker. Events appear when a blocker changes, so an absence of recent Events is normal.

For capacity policy, read `status.capacity.reason` and its explanation. `PendingCapacity` and `IneffectiveCapacity` point toward join or provisioning problems; `IncompleteMetrics` calls for Metrics Server and `/state` checks. See [capacity](../capacity/) and the [conditions reference](../../reference/conditions/).

## Optional Prometheus metrics

The chart can expose the controller metrics port through a ClusterIP Service. Enable `metrics.enabled=true` in chart values. `metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` require Prometheus Operator CRDs already installed. The included alert rules cover stalled operations and zero ready replicas. Restrict access to the metrics Service through cluster policy.

A Pod can stay `Running` after its celld child exits; the launcher keeps the
child stopped and readiness fails. See [runtime recovery](../../troubleshoot/recovery/)
for S3 lease expiry, delayed witnesses and the limits of administrative recovery.

Metrics and `Ready=True` are operational signals. Neither proves acknowledged writes survived, follower placement, or safe removal. A failed or ambiguous strict operation requires investigation; preserve the current reservation authority and affected storage. Follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/) before intervening.
