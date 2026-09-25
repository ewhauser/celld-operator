# Runtime responsibilities

The celld fork owns replication, tiering and recovery, and tolerates the loss
of any one member. Its node-log state reports, per member, durability posture,
follower-ensemble health and the last dead-leader sweep: unrecovered sessions
and which sessions still need each member's disk. The operator reads that state
to decide when the fleet has settled and when a removed member's disk may be
deleted; it does not second-guess recovery.

The following are explicit limits:

- All recovery participants need the fork's native `bucket_complete` reader.
- Settlement and disk release require node-log state (`0.5.1-ewhauser.5` or
  later). Unknown state delays voluntary changes and never releases a disk.
- A disk that is gone is replaced at once. celld recovers each dependent session
  from another complete copy or records a bounded loss; the operator only names
  those sessions.
- Storage deletion follows the Kubernetes contract in
  [retained disks](disposable-disks.md) and needs separate CSI/EBS tests.

See [qualification](qualification/README.md) for the checks actually run.
