# Capabilities and limitations

| Capability | Implemented behavior |
| --- | --- |
| Runtime | Explicit digest-pinned `ewhauser/celld` fork with strict shutdown schema 1; no default or stock-runtime fallback. |
| Workloads | Bucket Deployment or Ordered StatefulSet with temporary disk, running celld directly with `CELLD_DURABILITY=bucket`; PersistentFleet StatefulSet with CSI claims under the launcher. |
| Runtime safety | PersistentFleet: celld `data_safe` for the exact operation and generation, captured before process termination. Bucket: every acknowledged write is in the object store first, so losing any member loses no acknowledged write. |
| Process safety | PersistentFleet: launcher proves exact child exit, inherited-lock release and durable restart denial. Bucket: SIGTERM runs celld's drain, bounded by `lifecycle.shutdownSeconds`. |
| Kubernetes state | PersistentFleet: one bounded current operation and current resource bindings. Bucket: no current operation. No session archive or private S3 parsing. |
| Additions | Fresh Pods and, for PersistentFleet, fresh claims admitted against exact identities. |
| Contraction | One member at a time, each after the previous change has rolled out. PersistentFleet removes the highest ordinal after strict proof. Bucket Automatic/External contraction requires survivor capacity for every possible victim. |
| Maintenance | PersistentFleet: coordinated whole-fleet restart/upgrade with explicit downtime permission; strict shutdown before workload effects. Bucket: rolling restart/upgrade, one member at a time, no downtime permission. |
| Deletion | PersistentFleet strictly stops the current working set before removing compute; Bucket deletes the workload and members drain on SIGTERM. The bucket reservation is permanent. See the storage contract below for disk cleanup. |
| Public routing | Optional mutable HTTPRoute or Ingress to port 8080, explicit data-plane ingress policy, ownership-safe updates and cleanup. |
| Application visibility | Read-only observed versions, coverage and resident-cell convergence; unsupported or stale runtime observations remain Unknown. |
| Capacity | Manual targets, Shadow, ScaleOut, Automatic and External ownership all share the same executor and safety checks. |
| Kubernetes prerequisites | Services, NetworkPolicy and PodDisruptionBudget are created, then verified exactly. The only in-place change fills a field a newer release declares and the live object leaves unset, such as the peer port's `appProtocol: tcp`, pinned to the verified resourceVersion. Any other difference blocks as `InfrastructureBlocked`. Bucket fleets instead converge drift in their workload, NetworkPolicy and PodDisruptionBudget (`maxUnavailable: 1`); Services stay verify-only and objects without the fleet's UID label, or with owner references, are refused. |
| Failure handling | PersistentFleet: ambiguous or failed proof remains blocked. Status edits, timeouts and missing Pods cannot authorize removal. |

All nodes that may participate in recovery must run a compatible fork, including
readers for the native `bucket_complete` proof. A syntactically accepted image
pin does not establish release or recovery compatibility.

[Current operations](current-operation.md) specifies the storage contract and
cleanup completion boundary. [Test records](qualification/README.md) describe
the scenarios exercised and their environments. Verify your own runtime image,
CSI driver and recovery path before relying on them.

There is no migration from the former journal-based operator, no Deployment to
Ordered layout conversion, no automatic reuse of retained disks, no forced
storage-finalizer removal and no EC2 fencing override.
