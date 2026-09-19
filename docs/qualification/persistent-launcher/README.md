# Persistent launcher local evidence

Run on 18 September 2026 against unchanged celld v0.5.0 digest
`sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8`.
Operator baseline was `57124bb` plus the accompanying uncommitted implementation.

The [contract](../../persistent-fleet-lifecycle.md) defines the supported scope.
These are local Linux container and Kind results, not AWS, EKS, EBS CSI, RWOP,
or cross-host exclusion qualification. Production automatic contraction remains
disabled. Manual graceful removal requires at least two surviving members.

## Executed checks

- `make check`: builds, Go race tests, lint (zero issues).
- `make manifests-check`: generated artifacts match.
- `make qualification-replay`: five positive candidate traces and four expected
  blocked traces; replay is never live removal authority.
- `make qualification-test`: eight Python harness tests.
- Linux arm64 cross-compiled `go test` launcher suite executed in the pinned
  runtime image, including killed-supervisor/orphan inherited-lock exclusion.
  This Linux case is deliberately skipped by the macOS unit suite.
- Dockerfile build and packaged `/celld-launcher install /tmp/launcher-copy`
  smoke test passed using the distroless nonroot operator image.

## Real retained-volume exclusion

`hack/qualification/launcher_reuse.py` pauses the old runtime container and
starts a successor on the same retained Docker volume. The successor responds
`WaitingForExclusiveVolume`, child PID zero. The real celld process retains FD3
pointing to the volume lock. Only after removing the old process can the
successor acquire exclusivity, wait the restart spacing, and publish its new
generation. The generation equals the launcher's public key. See
[results](launcher-lock.json). This experiment contains no acknowledged write.

## Fault coverage and limits

Controller unit tests reload the durable journal between transitions, replay
lost launcher and replica-update responses, withhold follower retirement,
reject stale/missing/partial metadata, lost previously observed logs, generation
changes, node reboots, container restarts, claim/PV changes and incomplete
journal authority. Pause/cancel/loss fences also retain their shared controller
regression coverage. This is not an exhaustive distributed fault model or a
claim that every possible instruction boundary was fault-injected.

Uncertain node death, cross-host reuse, last-follower retirement, in-place legacy
RWO migration, upgrades and deletion remain outside the implemented executor.
No cloud resources or default kubeconfig were used. Harness-owned containers,
clusters, networks and volumes are removed by their cleanup handlers.

## Peer-only acknowledged write

`hack/qualification/persistent_peer_only.py` obtains a three-node ensemble, pauses
MinIO, and receives an acknowledged application write while object storage cannot
serve requests. It kills the owner before killing/restarting the paused store,
which drops any buffered owner PUT requests. It stops one of the two peer donors
through the authenticated launcher, resumes the surviving peer, and verifies
recovery and application readback. The final run recovered its one acknowledged
write in 16.198 seconds with no loss declaration and no missing operation IDs.
See [results](peer-only.json), [ledger](acknowledged-ledger.json), and the before
and after metadata files. The killed owner never resumes. This covers one real
peer-only write and failure ordering; it is not a broad workload durability
qualification or the uncertain-node controller path (which remains blocked).

## Bucket GC correction and remaining live-run limitation

The original `--bucket-lifecycle` run blocked after its first decrement on
`unresolved bucket writer record missing`; [its diagnostics](bucket-regression-failed.log)
are retained. Review identified an existing gap: positive expiry evidence was
not GC-safe until full settling completed. The correction durably preserves
that fact during settling and revokes it on observed renewal or replacement.
Regression tests cover GC, crash replay and contrary-evidence invalidation.

The [bounded follow-up run](../bucket-gc-review/README.md) passed manual 3→2→1,
growth, pause/resume across restart, and automatic 3→2. The full run remains
incomplete because multiple runtimes subsequently self-fenced on ambiguous
lease renewals and restarted; the changed generation correctly blocked further
contraction. The cause of those renew failures was not established. GC before
a first positive expiry read still blocks; no missing metadata bypass was added.

## StatefulSet lifecycle integration

The isolated three-node Kind fixture runs an in-cluster operator against the
unchanged pinned runtime, MinIO, Calico and a real Metrics Server. Two manual
3→2→3 cycles complete with controller restarts, acknowledged-write readback,
unchanged retained PVC UIDs, and reactivation on the original host. It exercises
actual stop requests, durable replica changes, scheduling gates and launcher
startup, rather than injecting completion attestations. Local storage is the
explicit RWO hostPath test exception. It does not establish RWOP/EBS behavior.

The final current-source run (`celld-step2-cd461a5f`) also passed live automatic
3→2 contraction using real Metrics Server data. See the complete
[integration log](persistent-integration.log), including durable journal state.
The earlier clean run (`celld-step2-74bc1082`) passed the same manual and automatic
sequence. This local automatic executor coverage does not remove the production
release gate.

Final cleanup verified no harness-owned `celld-step2-*` or `celld-q-*` containers,
qualification networks or volumes remained. The temporary operator image was
removed, and Docker VM `fs.inotify.max_user_instances` was restored to its original
128 after using 1024 for the disposable multi-node clusters. Unrelated running
containers were left untouched. The initial final-experiment attempt using a
`/tmp` bind mount failed because Docker resolved it as a directory; rerunning
with the repository's ignored `bin/` path succeeded and cleaned its resources.

## Parent review correction

The parent review reproduced a crash/pause bug: after an expansion replica CAS
succeeded but before `Reactivating` was persisted, the maintenance shortcut could
mark retained-volume reuse complete without checking the new invocation. The
pause guard now recognizes retained-volume expansion from durable history even
when the operation is still `Prepared`. The regression covers both phases with
valid creation and PVC metadata, so it reaches the relevant branch rather than
passing at an earlier provisioning guard. The `Prepared` case failed before the
fix and passed afterward under the race detector.
