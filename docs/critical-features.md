# Critical feature implementation status

This is the current implementation checklist. Earlier pre-release review tables
are historical snapshots, not the current feature matrix. Features below remain
experimental until their documented external qualification gates pass.

## Implemented

- [x] Strict-AZ Bucket membership with deterministic highest-ordinal retirement
  for new `bucketWorkload: Ordered` fleets. Uses disk-backed emptyDir, Pod UID
  runtime identity, explicit ordinal-zone scheduling and required host separation.
- [x] Qualified Bucket membership admission during normal operation, followed by
  exact generation succession after container restart, with persisted chains and
  live successor evidence. Unknown historical writers remain blocked.
- [x] Graceful same-AZ PersistentFleet PVC handoff across nodes: authenticated
  destination invocation/disk nonce, positively stopped predecessor, unchanged
  disk identity, and exclusive healthy RWOP gp2/gp3 EBS CSI attachment evidence.
- [x] Durable retired-Pod restart denial, authenticated in stop receipts, so a
  stale kubelet cannot restart a previously retired writer under the same Pod UID.
- [x] Same-version rolling restart tokens with exact UID deletion, persistent
  admission/recovery phases, fresh pre-stop checks and completed-token replay
  protection. PersistentFleet requires at least two survivors; strict restarts
  require the actual target removal to preserve AZ coverage and skew.
- [x] RetainData final shutdown and compute deletion, keeping storage and recovery
  authority. Bucket requires expired admitted sessions; PersistentFleet requires
  exact all-child stops and every positive own-log obligation sealed. A captured
  epoch-zero/no-log member is allowed only without contrary historical evidence.
- [x] Never-provisioned deletion creates a permanent reservation tombstone without
  inventing process evidence. A prior creation attempt cannot use this shortcut.
- [x] Journal v7, legacy reads, downgrade refusal, immutable integrity-checked
  archive pages and 16 MiB hydrated authority budget. No history is pruned.
- [x] Desired/applied/observed/ready/joining/terminating status, age/stall/loss
  metrics, blocker transition Events and per-fleet reconcile jitter.
- [x] Two-replica operator manifests and Helm chart, optional monitoring resources,
  canonical CRD/RBAC parity tests and immutable image/chart release packaging.

## Remaining implementation dependencies

- [ ] Cross-version celld upgrade and rollback: a second qualified runtime adapter
  and directional compatibility contract. Requests remain `UnsupportedTransition`;
  same-version restart is not advertised as upgrade support.
- [ ] Uncertain-node death/partition recovery: trusted infrastructure fencing or a
  runtime protocol that proves old filesystem access cannot resume. Missing Pods,
  expired S3 leases and force detach do not supply that authority.
- [ ] PersistentFleet 2-to-1 live contraction/restart: qualified follower retirement
  with fewer than two survivors. The existing positive donor seal is insufficient.
- [ ] Recovery from an all-stopped PersistentFleet shutdown whose best-effort
  seals did not complete. Disks and authority stay retained; an automatic
  recovery/retry protocol is not implemented.
- [ ] Legacy Deployment-to-Ordered and unwrapped/RWO-to-launcher/RWOP migrations:
  explicit identity and storage migration protocols. Existing workloads are not
  silently adopted or rewritten.
- [ ] Bucket GC-before-positive-proof: upstream node metadata GC can erase a record
  before expiry is positively observed. Missing metadata cannot resolve an unknown
  writer; delayed unconditional GC deletion also prevents broader safe adoption.

## Qualification and release gates

- [ ] Positive local rolling-restart/final-shutdown qualification: the attempted
  run stopped during a pinned-image network timeout before application testing.
- [ ] EKS, real S3/EBS CSI handoff, node/host failure and partitions.
- [ ] Production automatic contraction, real capacity-pool response and follower
  AZ diversity under failures. Local execution does not remove these gates.
- [ ] Runtime transition qualification after additional adapters exist.
- [ ] Owned API domain selection before a stable release (currently provisional
  `celld.example.com`).
- [ ] Published image/chart and registry verification: the workflow is implemented,
  but creating it does not publish a release.

See [maintenance](maintenance-execution.md), [storage handoff](persistent-fleet-lifecycle.md),
[Ordered Bucket](ordered-bucket.md), [journal archives](journal-archives.md), and
[operations](operations.md) for exact contracts and operational limits.
