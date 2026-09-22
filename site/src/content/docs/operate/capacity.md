---
title: Configure capacity policy
description: Observe demand recommendations, then enable bounded additions if the signals are complete.
---

Capacity policy reads each celld Pod's private `/state` endpoint and Kubernetes Metrics Server. Install Metrics Server and permit operator access to port 8081 before using it. You provision nodes, IAM roles, the CSI driver, and Metrics Server. Adding PersistentFleet replicas requests new PVCs through the configured StorageClass. Start with `Shadow` to see recommendations without changing the workload.

## Start in Shadow mode

On `my-fleet` in namespace `fleets`, apply the [shadow sample](../../api/samples/) after adapting its bounds to your zones. A minimal patch is:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet \
  --type merge -p '{"spec":{"capacity":{"mode":"Shadow","minReplicas":3,"maxReplicas":10}}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status.capacity'
```

Set `minReplicas` at least as high as `placement.azCount`. Defaults and tuning fields are in the [capacity policy fields](../../api/celldfleet/#speccapacity). A `Shadow` desired count is advice only. Confirm that `status.capacity` has fresh, complete observations from every expected replica, and compare the recommendation with actual workload demand before enabling actions.

## Enable bounded additions

Once the recommendation is useful, change only the mode:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet \
  --type merge -p '{"spec":{"capacity":{"mode":"ScaleOut"}}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status.capacity'
```

`ScaleOut` can add bounded capacity after a stable high-demand window. `spec.replicas` remains your manual target; the applied count can exceed it. `Automatic` also requests reductions. Every request uses the same strict current-operation executor as manual scaling; only Ordered Bucket and PersistentFleet support exact contraction. `External` instead assigns `spec.replicas` to one `/scale` writer and disables built-in demand collection.

`PendingCapacity` means a requested addition has not become useful. `IneffectiveCapacity` means it passed its provisioning deadline; inspect Pods, PVCs, and node capacity. `IncompleteMetrics` and `RepeatedSamples` reset stabilization. `ObservingRedistribution` waits to see whether additions relieve incumbents; `LoadNotRedistributed` holds further pressure-driven batches. The operator does not count an idle new Pod as useful redistribution. These reasons are explained in [conditions](../../reference/conditions/) and [capacity troubleshooting](../../troubleshoot/scheduling/).

Pause new policy actions during maintenance with `spec.maintenance.paused: true`; already issued work continues recovery. Removing the policy returns toward the manual replica target and can request a blocked reduction. Check the resulting target and [scaling procedure](../scaling/) first.
