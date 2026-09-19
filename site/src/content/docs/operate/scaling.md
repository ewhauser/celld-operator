---
title: Scale a fleet
description: Change the manual replica target and recognize safe progress or a blocked reduction.
---

Use this procedure to change `spec.replicas` on `my-fleet` in namespace `fleets`. Select the Kubernetes context explicitly and check the [placement rules](../../configure/placement/) before asking for more replicas. The operator provisions no nodes or cloud resources for you.

## Before you change the target

Check the current target, observed membership, and any operation already in progress:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o yaml
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
```

The new target must remain at least `placement.azCount` and within the API limit of 1–100. An existing [capacity policy](../capacity/) may request additional replicas; an edit to `spec.replicas` takes precedence over its next action. A previously issued action still finishes recovery. Avoid another change until that action is settled.

## Request and watch the change

For example, request four replicas:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet \
  --type merge -p '{"spec":{"replicas":4}}'
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -w
```

Inspect the detailed status in another terminal:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet \
  -o json | jq '.status'
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
```

For an increase, expect `desiredReplicas` to change first, followed by `appliedReplicas`, observed Pods, and `readyReplicas`. The operator records the change in the retained lifecycle journal. `Ready=True` is an availability observation, not proof of durable writes.

To decrease the target, change the same field to a smaller valid count. A Bucket fleet shrinks by one member, then waits for verified session expiry and healthy remaining members before completing the removal. A launcher-managed PersistentFleet stops the departing writer and checks its logs and remaining members before reducing the StatefulSet count. Some reductions require `maintenance.allowCoordinatedDowntime: true` and an intentional outage. Automatic production contraction remains blocked. See [profiles](../../concepts/profiles/) and [limitations](../../reference/limitations/) before requesting a reduction.

The change is complete when `status.appliedReplicas` and `status.readyReplicas` reach the requested count, `Progressing` is no longer active, and no `Blocked` reason remains for this request. During a reduction, `Ready=True` can describe the current workload size before the requested target is applied. If progress stops, read [lifecycle troubleshooting](../../troubleshoot/lifecycle/) and [scheduling troubleshooting](../../troubleshoot/scheduling/). Never edit the reservation journal or delete a Pod to bypass a stop gate.
