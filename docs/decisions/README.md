# Architecture decisions

[0022: celld owns runtime safety](0022-celld-control-plane.md) replaced the
former unmodified-runtime constraint, private-S3 recovery readers, lifecycle
journals and archives, operator EC2 fencing, version-specific offline adapters
and storage-reuse migrations. 0023 supersedes it.

Earlier records and their evidence remain in Git history. The current operator
has no compatibility path for fleets created with those implementations.

[0023: Node loss is routine](0023-node-loss-is-routine.md) replaces per-removal
proof with celld's tolerance of a lost node, removes the launcher, runs Bucket
fleets as plain rolling workloads, and keeps PersistentFleet disks across
restart and upgrade.

[0024: PersistentFleet is a StatefulSet](0024-persistentfleet-is-a-statefulset.md)
replaces 0023's PersistentFleet lifecycle. The StatefulSet's rolling update
restarts members, celld recovers them, every member keeps its disk until the
fleet is deleted, and the operator keeps no lifecycle state.
