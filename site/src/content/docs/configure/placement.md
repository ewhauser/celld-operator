---
title: Placement and capacity
description: Choose explicit zones, understand strict scheduling, and set a realistic replica floor.
sidebar:
  order: 5
---

Set placement.zones to one through six **unique, standard AZ names in storage.region** and set azCount to the list length. replicas must be at least azCount. The operator never changes the zone list or reselects zones when capacity disappears; placement is immutable after creation.

~~~yaml
spec:
  replicas: 3
  storage:
    bucket: dedicated-example-bucket
    region: us-east-1
    sizeGiB: 10
  placement:
    azCount: 3
    zones: [us-east-1a, us-east-1b, us-east-1c]
    mode: Strict
~~~

Strict is the default. It uses hard zone spread (maxSkew 1, DoNotSchedule) and distinct hostnames. With three zones and three replicas, supply an eligible node in **each** listed zone. More replicas need enough distinct eligible hosts to satisfy anti-affinity; otherwise Pods remain Pending. For Ordered Bucket and PersistentFleet, scheduling gates assign each ordinal its configured zone before launch. New PersistentFleet volumes bind in that zone.

Relaxed keeps the zone allowlist but makes spread and hostname separation preferences. It can place replicas unevenly or on a shared host, reducing fault isolation. Neither mode provisions nodes or fixes a missing zone. Check node labels, taints, capacity and Pod scheduling events before choosing Relaxed.

If you enable a capacity policy, capacity.minReplicas must be at least azCount. The policy can recommend additions and eligible contractions, but lifecycle evidence gates decide whether a change executes. Manual spec.replicas and capacity policy share the same bounded current operation. See [capacity policy](../../operate/capacity/) and [safety model](../../concepts/safety-model/).

## Size each replica before creation

Use `spec.execution` to set CPU and memory requests and limits for the celld container. Defaults are a `250m` CPU request with no CPU limit, a `512Mi` memory request, and a `1Gi` memory limit. Limits must be at least their requests. Optional `maxResidentCells` and `idleEvictSeconds` bound resident cells and idle hibernation; neither is a throughput guarantee.

Use `spec.lifecycle` to set shutdown time: `shutdownSeconds` defaults to 20 and `terminationGraceSeconds` to 30. The Pod termination grace must leave at least five seconds beyond the shutdown budget. A longer budget allows more time for handoff but does not prove that shutdown completed safely.

Both blocks are immutable after fleet creation. Choose values using application measurements before creating the fleet; existing fleets cannot adopt new settings through a template rollout. See the [tuned sample](../../api/samples/#tuned) and [API fields](../../api/celldfleet/#specexecution). Capacity policy thresholds use absolute CPU and memory values, so review them separately when choosing resource sizes.
