---
title: Verify and observe
description: Read fleet status, metrics and Events; know which signals are operational hints and which ones are safety evidence.
sidebar:
  order: 4
---

The operator reports through three channels: conditions and `status` on the `CelldFleet`, Kubernetes Events on blocker transitions, and optional Prometheus metrics. None of them is a recovery certificate; the journal on the storage reservation holds the authority.

## Status fields

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o jsonpath='{.status}' | jq
```

| Field | Meaning |
| --- | --- |
| `desiredReplicas`, `appliedReplicas`, `observedReplicas`, `readyReplicas`, `joiningReplicas`, `terminatingReplicas` | The replica pipeline from intent to observation. `replicaObservationValid=false` means the Pod list was incomplete and must not be read as zero running processes. |
| `conditions` | `Ready`, `InfrastructureReady`, `Blocked`, `LifecycleBlocked`, `LifecycleProgress`, `MaintenancePaused`, `Deleting`, `ProductionQualified`. See the [conditions reference](../../reference/conditions/). |
| `blockedSince` | When the current blocker began; use it to judge wait duration. |
| `lifecycle` | Operation ID, phase, from/to counts, selected target, start and completion timestamps, the sticky loss finding, retained blocked request (`requestKind`, `requestID`, `targetImage`) and `retiredBucketSessions`. |
| `capacity` | With a policy: recommended count, useful/pending/covered counts, mode, reason and explanation. In `Shadow` mode the recommendation is informational. |

## Events

Events are emitted only when a blocker's status or reason changes, so a quiet fleet produces none. Use `describe` to see them alongside conditions.

```bash
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
```

Pod events remain the place to distinguish insufficient nodes, zone constraints, PVC binding and runtime health.

## Metrics

Enable `metrics.enabled=true` in the chart to expose port 8084 through a ClusterIP Service, and the ServiceMonitor and PrometheusRule if Prometheus Operator is installed. Series are labelled by namespace and fleet and removed when deletion is observed. The bundled rules alert on stalled operations, possible loss and fleets with zero ready replicas. Restrict access to the metrics port with your cluster network policy.

## Signals that are not evidence

The safety model separates operational observations from fencing evidence. These common readings are the former:

- `Ready=True` says the workload reports runtime-ready replicas. It says nothing about durable writes or follower placement, and a paused or blocked fleet can stay Ready.
- A missing Pod, a terminated container or exit code 0 does not establish that a writer stopped. celld can exit successfully with unfinished durability work.
- A retained PVC is a recovery asset, not a sign of a clean shutdown.
- A cleared `status.lifecycle` does not cancel an operation. The reservation journal is authoritative.

When a sticky `possibleLoss` finding appears, investigate and recover; do not edit the journal to force progress. The [safety model](../../concepts/safety-model/) explains what the operator does accept as evidence.

## Local verification without AWS

The repository ships a disposable integration harness that creates its own kind cluster, installs Calico, MinIO and local-path volumes, and runs the pinned runtime against the operator. Nothing it proves transfers to EKS, S3 or EBS.

```bash
make integration
```

Profile-specific targets (`integration-bucket`, `integration-persistent`, `integration-ordered-bucket`, `integration-maintenance`, `integration-faults`, `integration-persistent-rwop`) run the in-cluster manager with Metrics Server and the local evidence transport. Recorded runs and their findings are under [qualification evidence](../../qualification/).
