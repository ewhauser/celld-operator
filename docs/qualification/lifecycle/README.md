# Step 3 lifecycle validation

18 September 2026. Step 2 commit `e29e193` was cherry-picked as `a4b9edb` in this
isolated worktree. Step 3 is uncommitted. No main or step 2 checkout was modified.

See [ADR 0012](../../decisions/0012-restart-safe-manual-lifecycle.md) for exact
journal semantics and qualification limits. The installed manager supports manual
scale-out in both profiles and explicitly blocks contraction in both profiles.
There is no production fence or Bucket no-log completion rule. The one-at-a-time
PersistentFleet contraction engine is exercised with injected local evidence,
not asserted to be a qualified live Kubernetes removal path.

## Checks

- `make check`: build, Go race tests, pinned golangci-lint; zero issues.
  [Captured check log](check.log).
- `make manifests-check`: pinned generator reproducibility.
- `make qualification-replay`: five positive candidate assessments, four expected
  blocks from the original real-runtime captures.
- `make qualification-test`: all eight Python collector tests.
- `go test -race ./internal/controller -run Lifecycle -count=1`: durable intent,
  controller reconstruction, status loss, post-replica-update crash, stale leader
  CAS, conflicting reservation updates, changed desired count, repeated single
  contractions, retained claim identity, ambiguous claim-allocation crash,
  incomplete listings, replacement generations, sticky historical loss,
  missing process fencing unavailable survivors, discarded historical inventory, settling reset and
  evidence expiring during membership revalidation. Contraction evidence and
  Kubernetes clients in these Go tests are synthetic; these are controller
  state-machine tests, not runtime or cloud qualification.
  [Captured lifecycle tests](lifecycle-tests.log); [qualification checks](qualification.log).

The prior adapter tests continue checking cancellation, stale observations,
invalid/duplicate JSON, pagination, missing logs, replaced/unresolved generations,
late-page historical loss and read ordering. `Inspect` reuses those same checks
for pre-removal assessments without requiring an already stopped session; a new
regression verifies that live preflight cannot masquerade as completion and
continues to reject historical loss.

## Local integration

`python3 hack/integration/run.py` passed all assertions; see [kind.log](kind.log).
Both profiles scaled from two to three real runtime replicas after controller
restart, with unchanged workload UIDs and templates. Both admitted contraction
requests were explicitly blocked without changing workload replicas. The suite
also verified the original isolation, strict missing-zone scheduling, drift,
pre-existing PVC rejection, retained disks, deletion gate and status reconstruction.
The unique cluster `celld-step2-2311bbee` was removed successfully; the harness's
historical step2 name prefix does not indicate use of an existing cluster.

 It uses only a
unique named cluster and generated kubeconfig, local MinIO and local-path PVCs,
the exact manager RBAC and the pinned unchanged runtime. No AWS resources or
pre-existing/default clusters are accessed. The harness removes its cluster and
operator process in a finally block. Docker image/build caches may remain.

## Unavailable paths

No live contraction, real node fencing, retained-EBS recovery, S3 IAM restrictions,
production S3 transport, sustained-pressure capacity test or AWS qualification is
claimed. Live `/state` collection, target/container identity validation and fencing
for contraction remain qualification-provider work. The private local test seam
cannot be enabled through flags, CR fields or annotations. PVC deletion,
reservation release, cleanup, upgrades and automatic capacity policy remain out
of scope. An unresolved operation deliberately does not time out into success.

## Review fixes: concurrent loss and pre-provisioning replica edits

Both reported defects were reproduced with failing regression tests before fixes.
Loss recording now first CASes a sticky marker onto the workload, the same object
that arbitrates replica issuance. A loss fence winning that CAS invalidates the
old issuer; an issuance winning first is already issued, and the marker prevents
further contraction. A crash between that marker and the reservation write cannot
forget the loss: reconciliation restores the journal from workload metadata.
Tests cover both CAS orderings, conflicting metadata updates and a controller
restart after the interrupted journal write. This is an authorization fence,
not the still-unqualified fencing of runtime processes on unreachable nodes.

Reservations now store the initial replica count atomically with their immutable
spec hash. Tests interrupt prerequisite creation, change replicas, reconstruct the
controller and finish provisioning in both profiles. Subsequent scale-out works;
immutable storage edits still fail validation against the original reservation.
The kind harness additionally exercises a replica edit while a conflicting
prerequisite prevents initial workload creation, using only resources created by
the harness invocation.

Review-fix check logs: [make check](review-fixes-check.log),
[manifest and qualification checks](review-fixes-qualification.log),
[lifecycle and regression tests](review-fixes-lifecycle-tests.log).

The [review-fix kind run](review-fixes-kind.log) passed all assertions, including
both profiles' restarted scale-out and the new pre-provisioning replica-edit case.
The unique cluster `celld-step2-9248149d` and its operator process were removed.
No AWS resources or pre-existing clusters were modified. Live contraction and
runtime process fencing remain unqualified and disabled in the shipped manager.
