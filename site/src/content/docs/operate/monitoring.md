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

Compare `desiredReplicas`, `appliedReplicas`, `observedReplicas`, and `readyReplicas` to find the stage that stopped. `replicaObservationValid=false` means the Pod list was incomplete; do not treat a zero count as proof that all processes stopped. `blockedSince` measures how long the current blocker has persisted. For PersistentFleet, a `LifecycleProgress` message says what the operator waits for: a rolling update, contraction or replacement waiting to settle, or a retained disk and the sessions that still need it. Warning Events `MemberDiskLost` and `MemberReplaced` record member replacements and name sessions celld may record as lost. Other Events appear when a blocker changes, so an absence of recent Events is normal.

For capacity policy, read `status.capacity.reason` and its explanation. `PendingCapacity` and `IneffectiveCapacity` point toward join or provisioning problems; `IncompleteMetrics` calls for Metrics Server and `/state` checks. See [capacity](../capacity/) and the [conditions reference](../../reference/conditions/).

## Optional Prometheus metrics

The chart can expose the controller metrics port through a ClusterIP Service. Enable `metrics.enabled=true` in chart values. `metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` require Prometheus Operator CRDs already installed. The included alert rules cover zero ready replicas; the stalled-operation rule does not fire because current releases record no operation deadline. Restrict access to the metrics Service through cluster policy.

See [runtime recovery](../../troubleshoot/recovery/) for S3 lease expiry,
delayed witnesses and lost disks.

Metrics and `Ready=True` are operational signals. Neither proves acknowledged writes survived or follower placement. Follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/) before intervening.
