# Runtime responsibilities

The celld fork owns replication, tiering and recovery, and tolerates the loss
of any one member. Its health endpoint, which is the readiness probe, returns
200 only after a node has recovered its previous session, and a new node holds
its first 200 until the fleet has absorbed the change. The StatefulSet paces
restarts on that readiness. The operator does not read celld's node-log state
and does not second-guess recovery; see
[PersistentFleet lifecycle](current-operation.md).

The following are explicit limits:

- All recovery participants need the fork's native `bucket_complete` reader.
- Pacing is readiness only. The operator does not wait for celld to finish
  recovering other sessions before the next restart.
- The operator replaces a member's disk only when its volume is gone, or when
  it is the one member down for the replacement delay while every other member
  has been ready for five minutes. It judges this from the member's Pod and
  claim alone. celld refuses the empty disk until the new member publishes its
  lease, then records a bounded loss for any session whose only complete copy
  was on the old disk.
- Two members down at once wait until one returns, unless a volume is lost.
  celld's guarantee covers the loss of one node.
- Storage deletion follows the Kubernetes contract in
  [retained disks](disposable-disks.md) and needs separate CSI/EBS tests.

See [qualification](qualification/README.md) for the checks actually run.
