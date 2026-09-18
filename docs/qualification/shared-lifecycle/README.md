# Shared lifecycle implementation validation

Worktree based on `d3ea0df`, 18 September 2026. Validation was completed before committing these changes.

## Provenance

The main checkout was read only. Its tested, uncommitted P2 corrections and
untracked review documentation were copied into this isolated worktree before
implementation. [The input hash manifest](inherited-p2.json) identifies every
inherited file. These include ready-but-idle addition holds and regression tests,
version-3 journals, four reconciliation workers, hostname anti-affinity,
readiness independent of maintenance blockers and the three-node kind harness.
Their earlier test logs remain in `../review-fixes/` and are not fresh results.

This task adds the S3 transport, production observational session inventory,
blocked production removal intents, survivor health/projection checks, operation
deadlines and cancellation protocol, journal version 4, evidence status fields,
fault tests and documentation. Existing lifecycle tests were updated only where
the new durable blocked-intent/version-4 behavior changes their expected state.
The main checkout was not modified, reset or committed during implementation.

Validation results are recorded below after the actual commands finish.

## Results from this worktree

- `make check`: passed build, all Go race tests and golangci-lint (zero issues).
  [Log](check.log). An initial race in the new HTTP test server's request counter
  was fixed with an atomic counter before the successful run.
- `make manifests-check`: passed. [Log](manifests.log).
- `make qualification-replay`: passed five candidate assessments and four
  expected blocks. [Log](replay.log).
- `make qualification-test`: passed all eight Python tests in this worktree's
  isolated environment. [Log](qualification-test.log).
- `make integration`: attempted; **failed before operator assertions** during
  kind worker join in `celld-step2-ee71a6e4`. The control plane started, but the
  worker Node did not register and kubeadm could not upload its CRI socket
  annotation. [Failure log](kind-failed.log). The harness cleaned up its cluster;
  the unrelated existing cluster was left untouched. No Docker VM sysctl or
  implicit/default kubeconfig was used or changed. This is not an integration
  pass, and the failure log does not establish a root cause.
- Python harness compilation and `git diff --check` passed.
- All input hashes and main-checkout HEAD still matched the provenance manifest
  at final verification. No commit or push was performed during validation. A subsequent user request authorized committing this work locally on main; no push was requested.

New tests exercise actual SigV4 S3 SDK requests to a local HTTP server, both
listing pages, access denial, missing/partial/oversized objects, malformed and
failed pagination, request and credential cancellation, endpoint override
rejection and scoped read restrictions. Controller tests cover generation
replacement, historical/missing/stale/partial inventory, durable sticky loss,
known identity reactivation denial, per-survivor CPU/memory projection and
uncertainty, canceled stale issuers, issuance winning cancellation, controller
restart at cancellation boundaries, expired unissued effects and recovery of
already issued effects after deadline. Existing synthetic contraction,
maintenance, capacity and P2 regression tests also pass.

No AWS/EKS/S3/EBS account was contacted. Production evidence was exercised with
fake Kubernetes clients and local S3 wire responses. Real signed generation
binding, process fencing, Metrics Server autoscaling, mode-specific removal,
retained-disk reactivation, upgrade and completed deletion remain unqualified or
unimplemented as described in [the contract](../../shared-lifecycle-safety.md).

## Commit handoff

The inherited P2 changes were committed separately on main as `9168bdd`. The
shared lifecycle changes follow in their own commit, preserving reviewable
provenance. The code and generated artifacts are identical to the validated
worktree; only this handoff documentation changed after validation.
