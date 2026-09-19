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
- [x] Journal v8, legacy reads, downgrade refusal, immutable integrity-checked
  archive pages and 16 MiB hydrated authority budget. No history is pruned.
- [x] Desired/applied/observed/ready/joining/terminating status, age/stall/loss
  metrics, blocker transition Events and per-fleet reconcile jitter.
- [x] Two-replica operator manifests and Helm chart, optional monitoring resources,
  canonical CRD/RBAC parity tests and immutable image/chart release packaging.

- [x] Opt-in EC2 termination for an exact, already-admitted PersistentFleet
  contraction donor that becomes unreachable. Durable intent, dedicated-host and
  retained-disk checks, and positive termination receipt; sealed logs and follower
  retirement remain required. This is not general node repair.
- [x] One-way v0.5.0 Bucket Deployment-to-Ordered migration with explicit downtime,
  retained reservation/history, positive expiry and durable replacement identity.

- [x] Manual PersistentFleet 2-to-1 contraction and small-fleet restart via explicit
  coordinated downtime, all-child stop receipts, all-own-log seals, a zero-Pod
  boundary and retained-disk successor admission. Live operation without downtime
  still requires two survivors; automatic 2-to-1 remains unavailable.
- [x] Exact released v0.4.1/v0.5.0 adapters and a directional PersistentFleet
  upgrade with explicit whole-fleet downtime, sealed logs, retained disks and
  a recoverable image-update CAS. See [runtime versions](runtime-versions.md).

## Remaining implementation dependencies

The released celld runtime must remain unmodified. Runtime-dependent gaps below
remain explicit unsupported paths; proposed runtime changes are out of scope.

- [ ] Runtime rollback and additional version pairs: no reverse storage-format
  contract exists for v0.5.0 to v0.4.1. Unsupported directions remain blocked.
- [ ] General uncertain-node recovery before authenticated identity admission, or
  after required Pod identity disappears. The implemented EC2 contraction fence
  covers only its explicitly captured donor; it does not infer unknown identities.
- [ ] Recovery from an all-stopped PersistentFleet shutdown whose best-effort
  seals did not complete. Disks and authority stay retained; an automatic
  recovery/retry protocol is not implemented.
- [ ] Legacy PersistentFleet unwrapped/RWO-to-launcher/RWOP conversion: explicit
  source-host fencing, offline disk identity initialization and retained-PV claim
  replacement protocol. Bucket layout migration is implemented separately.
- [ ] Bucket GC-before-positive-proof: upstream node metadata GC can erase a record
  before expiry is positively observed. Missing metadata cannot resolve an unknown
  writer; delayed unconditional GC deletion also prevents broader safe adoption.

## Qualification and release gates

- [ ] Positive local rolling-restart/final-shutdown qualification: the attempted
  run stopped during a pinned-image network timeout before application testing.
- [ ] EKS, real S3/EBS CSI handoff, node/host failure and partitions.
- [ ] Production automatic contraction, real capacity-pool response and follower
  AZ diversity under failures. Local execution does not remove these gates.
- [ ] Kubernetes runtime-transition orchestration qualification; local process
  compatibility experiments do not validate the full operator on EKS.
- [ ] Owned API domain selection before a stable release (currently provisional
  `celld.example.com`).
- [ ] Published image/chart and registry verification: the workflow is implemented,
  but creating it does not publish a release.

See [maintenance](maintenance-execution.md), [storage handoff](persistent-fleet-lifecycle.md),
[Ordered Bucket](ordered-bucket.md), [journal archives](journal-archives.md), and
[operations](operations.md), [EC2 fencing](infrastructure-fencing.md),
[Bucket migration](bucket-migration.md), and [runtime dependencies](runtime-dependencies.md)
for exact contracts and operational limits.
