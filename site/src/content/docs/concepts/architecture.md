---
title: Architecture
description: One controller, two profiles, a durable journal on a cluster-scoped reservation, and evidence transports that read but never write.
sidebar:
  order: 1
---

The operator follows [ADR 0001](../../decisions/0001-controller-architecture/): a single Go controller built on controller-runtime, reconciling a namespaced `CelldFleet` resource, with capacity recommendation separated from lifecycle execution. One installation manages many independent fleets across namespaces.

## Resources the operator owns

For each fleet the controller creates and drifts-checks:

| Resource | Bucket | PersistentFleet |
| --- | --- | --- |
| Workload | Deployment, or a StatefulSet with disk-backed `emptyDir` when `bucketWorkload: Ordered` | StatefulSet with one retained PVC per ordinal |
| Application Service | `<fleet>:8080`, ClusterIP | same |
| Peer Service | headless `<fleet>-peers` for DNS discovery; peers address individual Pod IPs | same |
| NetworkPolicy | 8080 open to Pods labelled `celld.example.com/client-of: <fleet>`; 8081 private to same-fleet peers and operator Pods | same, plus 8083 private to operator Pods for the launcher |
| PodDisruptionBudget | yes | yes |
| Launcher credential Secret | no | immutable per-fleet Secret with a random key |
| Journal archive ConfigMaps | immutable, content-addressed pages once the journal exceeds 180 KiB | same |

Cluster-wide, the operator creates one `CelldStorageReservation` per fleet. It is never garbage collected and never freed. See [lifecycle journal](../lifecycle-journal/).

## Reconciliation flow

1. **Admission and defaults.** CRD validation rules enforce immutability, zone and replica invariants; the controller re-checks names and dependencies and reports `Blocked` with a reason rather than failing silently.
2. **Reservation.** The bucket is bound to the fleet UID on the reservation before any namespaced object exists. A conflicting reservation blocks provisioning.
3. **Infrastructure.** Services, policy, budget, claims and the workload are created; drift against the exact expected template is reported, not repaired. Disruptive template repairs need a separately qualified migration.
4. **Observation.** The collector reads Pod inventory, runtime `/state` on port 8081 and Metrics Server, under a ten-second deadline with bounded workers. Incomplete observation is marked invalid rather than read as zero.
5. **Lifecycle.** Replica changes, maintenance requests and capacity decisions become journaled operations. Each effect is persisted before it is issued and recovered after a crash. See [safety model](../safety-model/).
6. **Status.** Conditions, replica pipeline counts, lifecycle projection and capacity explanation are written; Events fire on blocker transitions; metrics update.

## Components in the image

| Binary | Role |
| --- | --- |
| `celld-operator` | The controller. Two replicas with leader election; flags in the [operator flags reference](../../api/operator-flags/). |
| `celld-launcher` | Copied by an init container into an `emptyDir` and run as the celld container's entrypoint for PersistentFleet. Holds an exclusive lock on the volume, seeds the runtime's lease generation, serves authenticated stop requests on port 8083 and writes durable restart-denial markers. See [PersistentFleet lifecycle](../../contracts/persistent-fleet-lifecycle/). |

## External inputs

| Input | Used for | Mode |
| --- | --- | --- |
| Kubernetes API | everything above | read and write within granted RBAC |
| celld `/state` on 8081 | membership, health, pressure | read |
| S3 `nodes/` and `log/` under the fleet bucket | session inventory, lease expiry, loss declarations | read-only, separate identity |
| Metrics Server | capacity policy CPU and memory | read |
| EC2 `TerminateInstances` and `DescribeInstances` | opt-in fencing of one admitted contraction donor | write, only with both fencing flags |

The operator never writes to S3, never creates AWS resources, and never provisions nodes. Runtime service accounts receive no Kubernetes API tokens.

## Where the boundaries come from

- [ADR 0002](../../decisions/0002-durability-and-scaling/): both profiles, both scaling directions, in scope.
- [ADR 0003](../../decisions/0003-aws-platform-and-provisioning/): EKS, S3, EBS; provisioning stays external.
- [ADR 0004](../../decisions/0004-availability-zone-placement/): configurable AZ count with strict placement by default.
- [ADR 0006](../../decisions/0006-metrics-dependencies/): Prometheus optional.
- [ADR 0009](../../decisions/0009-service-and-ingress-boundary/): ClusterIP only; ingress, TLS, DNS external.
