# Architecture decisions

[0022: celld owns runtime safety](0022-celld-control-plane.md) describes the
current architecture. It replaces the former unmodified-runtime constraint,
private-S3 recovery readers, lifecycle journals and archives, operator EC2
fencing, version-specific offline adapters and storage-reuse migrations.

Earlier records and their evidence remain in Git history. The current operator
has no compatibility path for fleets created with those implementations.

[0023: Node loss is routine](0023-node-loss-is-routine.md) (proposed) replaces
per-removal proof with a one-disruption-at-a-time health gate, removes the
launcher, and keeps PersistentFleet disks across restart and upgrade.
