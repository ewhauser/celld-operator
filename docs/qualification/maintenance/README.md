# Step 5 maintenance validation

18 September 2026. Isolated worktree `faa1/celld-operator` started clean at
`39c12e7b8170e6fc7c77388931f271f03b5f1bce` (step 4), including step 3 `225fc33`.
Changes are uncommitted for review. Nothing was pushed or submitted as a PR.

See [ADR 0014](../../decisions/0014-coordinated-maintenance.md) for the API,
journal format, exact pause/deletion semantics and remaining qualification gates.
The matrix of qualified runtime transitions is empty. Neither upgrades, rollback,
planned restarts nor final shutdown is enabled. The existing two-profile
contraction blockers remain in place, including Bucket no-log interpretation,
process fencing and production recovery transport. `--local-test` is not a
production override.

## Regression and fault coverage

The added controller tests cover both profiles' pause/resume with a delayed
replica issuer, original target preservation, blocked request identity across
controller reconstruction, safe capacity after withdrawing a blocked request,
unsupported image/architecture-pin changes, unknown-source rollback/evidence
rejection, initial pause and initial unsupported pin, and explicit original pin
as a no-op. Concurrent deletion reconciles retain one request and freeze the
existing capacity operation. Configuration edits during an addition do not
retarget it or start an independent rollout.

Synthetic PersistentFleet recovery tests crash after replica issuance but before
journal advancement, then pause or delete. Recovery still completes with complete
synthetic evidence; missing evidence and historical loss cannot complete it.
PVC UIDs and session/history evidence remain. Issued additions are recorded
without another replica write. Legacy journal conversion preserves capacity and
claim identities; version 2 prevents older controllers silently ignoring requests.
These use fake Kubernetes clients and injected evidence, not actual process fences.

Three regressions were observed failing before their fixes:

- Pause retained cached actionable policy evidence. It now invalidates actionability
  and both positive windows, preserving source/action history.
- Retained PVCs with a garbage collection owner reference passed lifecycle checks.
  Changed ownership and reservation/label bindings now block further operations.
- A crash after writing only the pause fence, followed by resume, retained positive
  history. Resume now journals its reset before releasing the workload fence.

Existing capacity tests continue to verify that contrary/missing intermediate
observations reset stabilization without becoming positive samples or executable
cached recommendations. The pure capacity evaluator was not changed.

## Recorded checks

- `make check`: Go 1.27.1 build, all race tests and golangci-lint v2.13.2.
  [Log](check.log).
- `make manifests-check qualification-replay qualification-test`: generated
  schema/deepcopy reproducibility, five positive candidate replay assessments,
  four expected recovery blocks, eight Python collector tests.
  [Log](qualification.log).
- Focused race tests for maintenance, lifecycle, capacity, collector, policy,
  uncertainty and intermediate samples. [Log](focused-tests.log).
- Python integration script compilation and `git diff --check`.

## Disposable local integration

`make integration` passed all assertions with the final version-2 journal in
`celld-step2-e3298de0`; [log](kind.log). Both real runtime profiles started and
scaled out through controller restart without template changes. The suite
verified isolation, strict missing-zone scheduling, retained PVC identity,
unchanged contraction blockers, capacity behavior without Metrics Server,
maintenance fence acknowledgment, blocked restart and image requests, deletion
retention, status reconstruction and pre-provisioning replica edits. The actual
API server accepted the generated schemas and the exact manager RBAC sufficed.

An earlier run in `celld-step2-091fc6bf` also passed before the final journal-version
hardening. Both invocation-owned clusters and native operator processes were
removed. No pre-existing resource was cleaned up. Docker image/build caches remain.

## Limits

No AWS resources or pre-existing/default Kubernetes cluster were used. Local
MinIO and local-path claims do not qualify S3/EBS/IAM, retained-EBS recovery,
node fencing, correlated-loss durability, lease-aware restart, final shutdown,
version transitions or follower AZ diversity. Real successful metrics collection
and capacity decisions still use controlled HTTP/client fixtures; this kind
harness has no Metrics Server. The existing pressure-relief qualification limits
remain authoritative. Helm and release packaging remain step 6.

Deletion deliberately retains the finalizer even for absent or partially
provisioned workloads: no absence/timeout is a process-fencing certificate.
No cleanup, reservation transfer or disk adoption is enabled. Namespace or
privileged resource deletion is outside the supported retention contract; the
cluster-scoped reservation remains a tombstone against unsafe bucket reuse.

## Review fixes: withdrawal continuation and deletion status stability

Both findings were reproduced with failing regression tests before fixes.
Withdrawing a blocked restart or image request now schedules another reconcile
instead of depending on a previous leader's timer or an unrelated fleet event.
Tests cover both profiles and drive pending additions using only scheduled
continuations after leader reconstruction.

Request persistence no longer reports an intermediate deletion status. The
maintenance path reports its final outcome once, after any in-flight recovery
assessment. Regression tests check idle deletion, frozen unissued capacity and
issued recovery with incomplete evidence: each transition writes at most one
status, and repeated unchanged reconciles write none. Delete intent, retained
claims and unresolved recovery remain intact.

Validation for these fixes: [make check](review-fix-check.log) and
[manifest/replay/qualification checks](review-fix-qualification.log).
The Go race suite includes the new regression tests and existing lifecycle and
capacity tests. The earlier disposable kind runs were not repeated for these
reconcile control-flow fixes; no additional cluster or AWS resources were used.
