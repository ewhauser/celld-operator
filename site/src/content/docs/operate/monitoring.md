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

Compare `desiredReplicas`, `appliedReplicas`, `observedReplicas`, and `readyReplicas` to find the stage that stopped. `replicaObservationValid=false` means the Pod list was incomplete; do not treat a zero count as proof that all processes stopped. `blockedSince` measures how long the current blocker has persisted. While a change rolls out, the `Provisioning` message says what the fleet waits for: updated, ready replicas, the end of the previous rollout before the next scale-in step, or a down member and the time it will be replaced on a fresh disk, with the scheduler's reason when the member cannot be scheduled. `MemberUnschedulable` sets `Blocked` when such a member is still unplaced after the replacement delay and the operator does not replace it. `LifecycleProgress` appears while the operator heals a member or deletes the fleet; its message names the action. Warning Events `MemberForceDeleted`, `MemberDiskLost` and `MemberReplaced` record each [self-healing](../../concepts/current-operation/#self-healing) action. An `OperationSuperseded` Event records an in-flight operation from an earlier release that was dropped on upgrade. Other Events appear when a blocker changes, so an absence of recent Events is normal.

For capacity policy, read `status.capacity.reason` and its explanation. `PendingCapacity` and `IneffectiveCapacity` point toward join or provisioning problems; `IncompleteMetrics` calls for Metrics Server and `/state` checks. See [capacity](../capacity/) and the [conditions reference](../../reference/conditions/).

## Optional Prometheus metrics

The chart can expose the controller metrics port through a ClusterIP Service. Enable `metrics.enabled=true` in chart values. `metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` require Prometheus Operator CRDs already installed. The included alert rules cover a blocker reported for more than 15 minutes and zero ready replicas. Restrict access to the metrics Service through cluster policy.

Agents that discover scrape targets from Pod annotations need no Prometheus Operator. Set them with `podAnnotations`, which the chart places on the operator Pod template. Values must be strings. For the Datadog Agent, an OpenMetrics check on the `operator` container looks like this:

```yaml
metrics:
  enabled: true
podAnnotations:
  ad.datadoghq.com/operator.checks: |
    {"openmetrics": {"instances": [{
      "openmetrics_endpoint": "http://%%host%%:8084/metrics",
      "namespace": "",
      "metrics": ["celld_fleet_.*"]
    }]}}
```

Every operator replica serves metrics, but only the elected leader reconciles, so only the leader reports `celld_fleet_*` and per-controller series. Standbys serve process and client metrics alone. Aggregate fleet series across operator Pods with `max`, not `sum`.

### Capacity policy metrics

`celld_fleet_desired_replicas` follows the capacity policy only in `ScaleOut` and `Automatic` modes; otherwise it is `spec.replicas`. The policy's own decision is published in every mode, including `Shadow`, from `status.capacity`:

| Metric | Value |
| --- | --- |
| `celld_fleet_capacity_recommended_replicas` | `desiredReplicas`, the count the policy recommends |
| `celld_fleet_capacity_useful_replicas` | `usefulReplicas` |
| `celld_fleet_capacity_pending_replicas` | `pendingReplicas` |
| `celld_fleet_capacity_covered_replicas` | `coveredReplicas` |
| `celld_fleet_capacity_decision{reason}` | 1 for the current `reason`, 0 for every other |

`reason` takes the fixed set the policy emits; a reason this release does not know reports as `Other`. A fleet has no capacity series when `spec.capacity` is unset, its mode is `External`, or the policy has not decided yet, so an absent series never reads as a recommendation of zero.

Comparing the recommendation to the applied count tells you how a fleet is sized in any mode. A positive value means it is underprovisioned, a negative value overprovisioned:

```promql
max by (namespace, fleet) (celld_fleet_capacity_recommended_replicas)
  - max by (namespace, fleet) (celld_fleet_applied_replicas)
```

This is how to judge a fleet in `Shadow` mode before handing it `ScaleOut`. For example, alert when the difference stays above zero for 15 minutes, and send it to a low-urgency channel when it stays below zero for a day. The reason behind a hold:

```promql
max by (namespace, fleet, reason) (celld_fleet_capacity_decision) == 1
```

#### Example Grafana panel

This time series panel plots the policy's recommendation against the applied and ready counts for one fleet, so a gap between the lines shows how far the fleet is from what the policy wants. It assumes a Prometheus data source and dashboard variables `namespace` and `fleet`, for example `label_values(celld_fleet_applied_replicas, namespace)` and `label_values(celld_fleet_applied_replicas{namespace="$namespace"}, fleet)`. Paste it into a dashboard through **Add > Visualization**, then the panel JSON editor.

```json
{
  "type": "timeseries",
  "title": "Capacity: recommended vs applied",
  "datasource": {"type": "prometheus", "uid": "${datasource}"},
  "fieldConfig": {
    "defaults": {"unit": "short", "decimals": 0, "custom": {"lineInterpolation": "stepAfter"}},
    "overrides": [
      {"matcher": {"id": "byName", "options": "recommended"},
       "properties": [{"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [10, 10]}}]}
    ]
  },
  "targets": [
    {"refId": "A", "legendFormat": "recommended",
     "expr": "max(celld_fleet_capacity_recommended_replicas{namespace=\"$namespace\", fleet=\"$fleet\"})"},
    {"refId": "B", "legendFormat": "applied",
     "expr": "max(celld_fleet_applied_replicas{namespace=\"$namespace\", fleet=\"$fleet\"})"},
    {"refId": "C", "legendFormat": "ready",
     "expr": "max(celld_fleet_ready_replicas{namespace=\"$namespace\", fleet=\"$fleet\"})"},
    {"refId": "D", "legendFormat": "pending",
     "expr": "max(celld_fleet_capacity_pending_replicas{namespace=\"$namespace\", fleet=\"$fleet\"})"}
  ]
}
```

To see why the recommendation holds, add a **State timeline** panel with the query `max by (reason) (celld_fleet_capacity_decision{namespace="$namespace", fleet="$fleet"}) == 1` and legend `{{reason}}`; each band is how long the policy reported that reason.

#### Example alert rules

The chart's `metrics.prometheusRule` covers only blockers and unavailable fleets. These capacity rules go in a separate `PrometheusRule`, or the equivalent `groups` block of a plain Prometheus rule file. Adjust the `for` durations and severities to your paging policy.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: celld-fleet-capacity
  namespace: celld-system
spec:
  groups:
    - name: celld-fleet-capacity
      rules:
        - alert: CelldFleetUnderprovisioned
          expr: |
            max by (namespace, fleet) (celld_fleet_capacity_recommended_replicas)
              - max by (namespace, fleet) (celld_fleet_applied_replicas) > 0
          for: 15m
          labels: {severity: warning}
          annotations:
            summary: Capacity policy recommends more replicas than {{ $labels.namespace }}/{{ $labels.fleet }} runs
        - alert: CelldFleetOverprovisioned
          expr: |
            max by (namespace, fleet) (celld_fleet_capacity_recommended_replicas)
              - max by (namespace, fleet) (celld_fleet_applied_replicas) < 0
          for: 1d
          labels: {severity: info}
          annotations:
            summary: "{{ $labels.namespace }}/{{ $labels.fleet }} runs more replicas than its capacity policy recommends"
        - alert: CelldFleetCapacityMetricsIncomplete
          expr: max by (namespace, fleet) (celld_fleet_capacity_decision{reason="IncompleteMetrics"}) == 1
          for: 15m
          labels: {severity: warning}
          annotations:
            summary: Capacity policy for {{ $labels.namespace }}/{{ $labels.fleet }} lacks the metrics it needs to decide
```

A fleet whose policy is unset, `External`, or not yet decided has no capacity series, so these rules never fire for it. The rules read differently by mode. In `Shadow`, they tell you what the policy would change. In `ScaleOut` and `Automatic` the operator applies additions itself, so a sustained `CelldFleetUnderprovisioned` means an addition is not landing; check `status.capacity.reason` and the `Provisioning` condition. `ScaleOut` never contracts, so `CelldFleetOverprovisioned` there only says a smaller fleet would do; route it accordingly or scope the rule with a `fleet` matcher.

See [runtime recovery](../../troubleshoot/recovery/) for S3 lease expiry,
delayed witnesses, lost nodes and lost disks.

Metrics and `Ready=True` are operational signals. Neither proves acknowledged writes survived or follower placement. Follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/) before intervening.
