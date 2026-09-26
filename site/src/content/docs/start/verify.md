---
title: Verify and troubleshoot
description: Check rollout, conditions, Pod events and runtime logs when a fleet does not become ready.
sidebar:
  order: 6
---

Start with the workload and fleet status. All commands name the kubeconfig context explicitly.

```bash
kubectl --context YOUR_CONTEXT -n celld-system rollout status deployment/celld-celld-operator --timeout=180s
kubectl --context YOUR_CONTEXT -n fleets get statefulset/my-fleet,service/my-fleet
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
kubectl --context YOUR_CONTEXT -n fleets get pods -o wide
```

A healthy first run has the operator Deployment available, three ready runtime Pods, a `my-fleet` Service on port 8080, and `Ready=True` on the fleet. `Ready` follows the current workload's applied replicas and generation; compare it with `status.desiredReplicas` and `status.appliedReplicas` when a change is in progress. Conditions and Events are operational observations, not proof that a previous writer was safely stopped or an acknowledged write survived a failure.

| Symptom | Check next |
| --- | --- |
| `Blocked=True`, `NamespaceAccessDenied` | Check `fleets` RoleBinding and its subject. For Helm release `celld`, it must name `celld-system/celld-celld-operator`. |
| `Blocked=True`, NetworkPolicy not attested | Verify CNI enforcement, then set chart `networkPolicyEnforced=true`. |
| Pods Pending | `kubectl --context YOUR_CONTEXT -n fleets describe pod POD_NAME` shows zone, capacity or pull errors. Strict placement will not silently relax. |
| Pods start but are not ready | Inspect Pod logs and bucket access. For PersistentFleet see [runtime recovery](../../troubleshoot/recovery/). |
| `Ready=True` but application request fails | Confirm `celld deploy` used the fleet bucket, inspect runtime logs, and use an authorized client label for in-cluster traffic. |
| `Blocked=True`, storage reservation conflict | Use a new, dedicated bucket. A reservation is bound to a fleet UID and is not released by routine deletion. |

For a focused status view:

```bash
kubectl --context YOUR_CONTEXT -n fleets get celldfleet my-fleet -o json | jq '.status.conditions'
kubectl --context YOUR_CONTEXT -n fleets logs statefulset/my-fleet --tail=100
kubectl --context YOUR_CONTEXT -n celld-system logs deployment/celld-celld-operator --tail=100
```

Events appear when blocker state changes, so a quiet fleet may have no new Events. The [conditions reference](../../reference/conditions/) defines the current types and reasons. Optional Prometheus metrics are configured through [chart values](../../api/helm-values/); Metrics Server is needed for capacity recommendations, not for this first fleet.

The repository's disposable `make integration` harness runs with kind and MinIO. It is useful to test the implementation locally; its Kind storage driver differs from EBS. Do not use `--local-test` against EKS. If a lifecycle change keeps waiting, preserve the storage reservation and affected disks and follow [lifecycle troubleshooting](../../troubleshoot/lifecycle/).
