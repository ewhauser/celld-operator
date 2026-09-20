# Runtime responsibilities

The celld fork owns replication, tiering, recovery and the decision that a node's
local disk is disposable. Its strict shutdown quiesces application and follower
admission and resolves both its own and follower obligations before reporting
`data_safe`. The controller consumes that public result through the launcher.

The launcher captures the result while celld is still alive in control-only
mode, then proves exact process termination and restart exclusion. The operator
retains proof only for the current infrastructure operation.

The following are explicit limits:

- All recovery participants need the fork's native proof reader.
- A runtime/launcher crash before Kubernetes captures proof can leave removal
  blocked indefinitely. The minimum contract is not a durable runtime job service.
- An unrequested process exit or failed strict operation provides no deletion
  authority, even when the process is gone.
- A local flock cannot prove cross-kernel exclusion. Cross-host/boot reuse is
  blocked; no operator handoff grant or EC2 termination repairs the missing proof.
- Storage deletion follows the exact Kubernetes contract in
  [current operations](current-operation.md) and needs separate CSI/EBS tests.

See [qualification](qualification/README.md) for the checks actually run.
