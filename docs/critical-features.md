# Capabilities and limitations

| Capability | Implemented behavior |
| --- | --- |
| Runtime | Explicit digest-pinned `ewhauser/celld` fork. Every fork build rolls the same way; the operator reads no node-log state. No default or stock-runtime fallback. |
| Workloads | Both profiles run celld directly; there is no launcher. Bucket: Deployment or Ordered StatefulSet with temporary disk and `CELLD_DURABILITY=bucket`. PersistentFleet: StatefulSet with retained RWOP CSI claims and `CELLD_DURABILITY=fleet`. |
| Runtime safety | celld tolerates the loss of any one member. PersistentFleet: the StatefulSet restarts one member at a time and waits for it to be Ready, and celld recovers each member from its own disk and its peers. Bucket: every acknowledged write is in the object store first. |
| Process safety | SIGTERM runs celld's drain, bounded by `lifecycle.shutdownSeconds`. |
| Kubernetes state | Only capacity-policy history, on the reservation, removed when there is none. No current operation, session archive or private S3 parsing. |
| Additions | Applied in one step, with fresh Pods. A PersistentFleet ordinal that had a member reattaches its kept claim, which celld treats as a restart; new ordinals get new claims. A claim at a member's name without the fleet's UID label blocks growth as `StorageIdentityConflict`. |
| Contraction | One member at a time, each after the previous change has rolled out. PersistentFleet removes the highest ordinal and keeps its claim, which costs storage until growth reattaches it or the fleet is deleted. Automatic/External contraction requires survivor capacity. |
| Maintenance | Rolling restart/upgrade, one member at a time, no downtime permission. PersistentFleet keeps disks and restarts the highest ordinal first. Pacing is readiness only, and a rollout does not wait for another member that is already down, so two members can be down at once. |
| Lost disks | Replaced by the operator with no manual step. A claim in phase `Lost` is deleted with its Pod at once. One member down for the replacement delay (10 minutes by default, `--member-replacement-delay`) while every other member has been ready for five minutes has its claim and Pod deleted. Either way the StatefulSet creates a fresh claim, and celld records a bounded loss for sessions whose only copy was on the old disk. A member waiting on its image or configuration, or already on a disk created for its current Pod, is left alone. Two members down at once wait until one returns, unless a volume is lost. |
| Deletion | The workload is deleted in the foreground and members drain on SIGTERM; PersistentFleet then deletes every claim, including those of removed members. The bucket reservation is permanent. |
| Public routing | Optional mutable HTTPRoute or Ingress to port 8080, explicit data-plane ingress policy, ownership-safe updates and cleanup. |
| Application visibility | Read-only observed versions, coverage and resident-cell convergence; unsupported or stale runtime observations remain Unknown. |
| Capacity | Manual targets, Shadow, ScaleOut, Automatic and External ownership all share the same executor and safety checks. |
| Kubernetes prerequisites | Services are created, then verified exactly. Their only in-place change fills a field a newer release declares and the live object leaves unset, such as the peer port's `appProtocol: tcp`, pinned to the verified resourceVersion. Any other difference blocks as `InfrastructureBlocked`. Both profiles converge drift in their workload, NetworkPolicy and PodDisruptionBudget; objects without the fleet's UID label, or with owner references, are refused. A hand edit to the StatefulSet template starts rolling at once, and the operator restores its template on the next reconcile. The budget is a fixed `maxUnavailable: 1`. |
| Failure handling | The operator takes at most one self-healing action per reconcile, judged from Pod and claim state, and records nothing. A member Pod still present more than two minutes after its termination grace ended is force-deleted, in every StatefulSet fleet, and the member keeps its disk; celld's lease fences a process left on the lost node. Fleet status edits and missing Pods cannot authorize deleting an existing disk. A member that cannot come back holds a rollout or contraction for at most the replacement delay; a lost volume holds nothing up. |

All nodes that may participate in recovery must run a compatible fork, including
readers for the native `bucket_complete` proof. A syntactically accepted image
pin does not establish release or recovery compatibility.

[PersistentFleet lifecycle](current-operation.md), including its
[limits](current-operation.md#limits), and [retained disks](disposable-disks.md)
specify the lifecycle and storage contract. [Test records](qualification/README.md) describe
the scenarios exercised and their environments. Verify your own runtime image,
CSI driver and recovery path before relying on them.

There is no migration from the former journal-based operator, no Deployment to
Ordered layout conversion, no adoption of another fleet's disk, no forced
storage-finalizer removal and no EC2 fencing override.
