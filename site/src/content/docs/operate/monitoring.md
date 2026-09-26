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

Compare `desiredReplicas`, `appliedReplicas`, `observedReplicas`, and `readyReplicas` to find the stage that stopped. `replicaObservationValid=false` means the Pod list was incomplete; do not treat a zero count as proof that all processes stopped. `blockedSince` measures how long the current blocker has persisted. While a change rolls out, the `Provisioning` message says what the fleet waits for: updated, ready replicas, the end of the previous rollout before the next scale-in step, or a down member and the time it will be replaced on a fresh disk. `LifecycleProgress` appears while the operator heals a member or deletes the fleet; its message names the action. Warning Events `MemberForceDeleted`, `MemberDiskLost` and `MemberReplaced` record each [self-healing](../../concepts/current-operation/#self-healing) action. An `OperationSuperseded` Event records an in-flight operation from an earlier release that was dropped on upgrade. Other Events appear when a blocker changes, so an absence of recent Events is normal.

For capacity policy, read `status.capacity.reason` and its explanation. `PendingCapacity` and `IneffectiveCapacity` point toward join or provisioning problems; `IncompleteMetrics` calls for Metrics Server and `/state` checks. See [capacity](../capacity/) and the [conditions reference](../../reference/conditions/).

## Optional Prometheus metrics

The chart can expose the controller metrics port through a ClusterIP Service. Enable `metrics.enabled=true` in chart values. `metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` require Prometheus Operator CRDs already installed. The included alert rules cover a blocker reported for more than 15 minutes and zero ready replicas. Restrict access to the metrics Service through cluster policy.

See [runtime recovery](../../troubleshoot/recovery/) for S3 lease expiry,
delayed witnesses, lost nodes and lost disks.

Metrics and `Ready=True` are operational signals. Neither proves acknowledged writes survived or follower placement. Follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/) before intervening.
