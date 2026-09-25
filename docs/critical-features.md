# Capabilities and limitations

| Capability | Implemented behavior |
| --- | --- |
| Runtime | Explicit digest-pinned `ewhauser/celld` fork; PersistentFleet settlement and disk release need `0.5.1-ewhauser.5` node-log state. No default or stock-runtime fallback. |
| Workloads | Both profiles run celld directly; there is no launcher. Bucket: Deployment or Ordered StatefulSet with temporary disk and `CELLD_DURABILITY=bucket`. PersistentFleet: StatefulSet with retained RWOP CSI claims and `CELLD_DURABILITY=fleet`. |
| Runtime safety | celld tolerates the loss of any one member. PersistentFleet: the operator disrupts one member at a time and waits until every member's node-log report shows the fleet settled. Bucket: every acknowledged write is in the object store first. |
| Process safety | SIGTERM runs celld's drain, bounded by `lifecycle.shutdownSeconds`. |
| Kubernetes state | Applied count, image, restart token, last disruption and capacity-policy state on the reservation. No current operation, session archive or private S3 parsing. |
| Additions | Fresh Pods and, for PersistentFleet, fresh claims. Growth never reuses a removed member's claim. |
| Contraction | One member at a time, each after the previous change has rolled out. PersistentFleet removes the highest ordinal when settled and deletes its claim only once no session needs it. Automatic/External contraction requires survivor capacity. |
| Maintenance | Rolling restart/upgrade, one member at a time, no downtime permission. PersistentFleet keeps disks and replaces a running member only when settled. |
| Lost disks | PersistentFleet replaces a member whose claim is `Lost` or whose PV is gone at once, and names the sessions celld may record as lost. `celld.eric.dev/replace-member` does the same on request. |
| Deletion | The workload is deleted in the foreground and members drain on SIGTERM; PersistentFleet then deletes its claims. The bucket reservation is permanent. |
| Public routing | Optional mutable HTTPRoute or Ingress to port 8080, explicit data-plane ingress policy, ownership-safe updates and cleanup. |
| Application visibility | Read-only observed versions, coverage and resident-cell convergence; unsupported or stale runtime observations remain Unknown. |
| Capacity | Manual targets, Shadow, ScaleOut, Automatic and External ownership all share the same executor and safety checks. |
| Kubernetes prerequisites | Services, NetworkPolicy and PodDisruptionBudget are created, then verified exactly. The only in-place change fills a field a newer release declares and the live object leaves unset, such as the peer port's `appProtocol: tcp`, pinned to the verified resourceVersion. Any other difference blocks as `InfrastructureBlocked`. Both profiles converge drift in their workload, NetworkPolicy and PodDisruptionBudget; Services stay verify-only and objects without the fleet's UID label, or with owner references, are refused. The budget is `maxUnavailable: 1`, or `0` while a PersistentFleet recovers. |
| Failure handling | Unknown settlement delays only the next voluntary step. Status edits, timeouts and missing Pods cannot authorize deleting an existing disk. |

All nodes that may participate in recovery must run a compatible fork, including
readers for the native `bucket_complete` proof. A syntactically accepted image
pin does not establish release or recovery compatibility.

[One disruption at a time](current-operation.md) and [retained disks](disposable-disks.md)
specify the lifecycle and storage contract. [Test records](qualification/README.md) describe
the scenarios exercised and their environments. Verify your own runtime image,
CSI driver and recovery path before relying on them.

There is no migration from the former journal-based operator, no Deployment to
Ordered layout conversion, no reuse of a removed member's disk, no forced
storage-finalizer removal and no EC2 fencing override.
