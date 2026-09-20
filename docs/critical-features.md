# Capabilities and limitations

The operator is experimental and requires `qualification: Experimental`.
`ProductionQualified=False` reports the outstanding cloud qualification.

| Capability | Implemented behavior |
| --- | --- |
| Runtime | Explicit digest-pinned `ewhauser/celld` fork with strict shutdown schema 1; no default or stock-runtime fallback. |
| Workloads | Bucket Deployment or Ordered StatefulSet with temporary disk; PersistentFleet StatefulSet with CSI claims. Both use the launcher. |
| Runtime safety | celld `data_safe` for the exact operation and generation, captured before process termination. |
| Process safety | Launcher proves exact child exit, inherited-lock release and durable restart denial. |
| Kubernetes state | One bounded current operation and current resource bindings; no session archive or private S3 parsing. |
| Additions | Fresh Pods and, for PersistentFleet, fresh claims admitted against exact identities. |
| Contraction | One highest ordinal at a time for StatefulSets. Bucket Deployment contraction is blocked because Kubernetes chooses its victim. |
| Maintenance | Coordinated whole-fleet restart/upgrade with explicit downtime permission; strict shutdown before workload effects. |
| Deletion | Strictly stop the current working set before removing compute. The bucket reservation is permanent. See the storage contract below for disk cleanup. |
| Capacity | Manual targets, Shadow, ScaleOut, Automatic and External ownership all share the same executor and safety checks. |
| Failure handling | Ambiguous or failed proof remains blocked. Status edits, timeouts and missing Pods cannot authorize removal. |

All nodes that may participate in recovery must run a compatible fork, including
readers for the native `bucket_complete` proof. A syntactically accepted image
pin does not establish release or recovery compatibility.

[Current operations](current-operation.md) specifies the storage contract and
cleanup completion boundary. [Qualification](qualification/README.md) records
local tests and remaining integration and cloud gates. Local unit tests,
MinIO runs and simulated CSI objects do not qualify EKS/EBS, cross-host disk
reuse or failure-domain recovery.

There is no migration from the former journal-based operator, no Deployment to
Ordered layout conversion, no automatic reuse of retained disks, no forced
storage-finalizer removal and no EC2 fencing override.
