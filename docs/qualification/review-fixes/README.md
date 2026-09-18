# Four P2 review corrections

18 September 2026, local changes based on `d3ea0df`. These address the four
findings in [the pre-release review](../../pre-release-review.md). Broader
implementation and AWS qualification gaps from that review remain open.

All four regressions were observed failing before their fixes:

- An unchanged hot pod drove three replicas to ten while every addition was idle.
  The policy now retains load baselines and observes each addition for a configurable
  continuous window (120 seconds by default). Two ineffective batches stop further
  pressure-driven additions with `LoadNotRedistributed`. The regression now stops
  at five replicas and preserves the hold through JSON reconstruction after every
  observation. Further tests cover sustained CPU demand growth, redistribution,
  transient improvements, intermediate contrary observations, missing samples,
  replaced identities, idle memory caches and reaching an explicit minimum.
- A blocked reconciliation prevented a second fleet from running. The real
  controller-runtime work queue test now processes another fleet while the first
  remains blocked. Production registration uses the tested four-worker options.
- Strict templates had no hostname separation. Both profiles now require
  same-fleet hostname anti-affinity; Relaxed makes it a preference while retaining
  the zone allowlist. Unit tests cover both settings, and kind verifies placement
  on distinct nodes before and after scale-out.
- Pause and blocked restart/image requests erased observed availability. Both
  profiles now retain their actual ready count and Ready condition while exposing
  their lifecycle blocker separately. Unit tests cover all three requests; the
  live suite checks both profiles remain Ready with three replicas while paused.

## Checks

- `make check` passed: build, race tests and zero lint issues. [Log](check.log).
- `make manifests-check`, `make qualification-replay`, and
  `make qualification-test` passed: generated artifacts match, five positive
  offline assessments and four expected blocks, eight Python tests.
- `make integration` passed in disposable cluster `celld-step2-54e88174`.
  [Log](kind.log). The suite uses three nodes in one test AZ, actual pinned celld,
  Calico, MinIO and local-path storage. Both profiles started and expanded from two
  to three distinct hosts through controller restart; isolation, existing-PVC
  rejection, missing-zone blocking, drift detection, maintenance readiness,
  retained deletion and reservation reconstruction assertions passed.
- Python compilation and `git diff --check` passed.

The Docker VM initially exhausted its inotify instance limit while bootstrapping
three kubelets alongside other local workloads. The limit was temporarily raised
from 128 to 1024, then restored to 128 after the successful run. The harness does
not change that setting. Its unique cluster and native operator were removed;
other running clusters were left untouched.

## Compatibility and limits

The stronger affinity changes generated pod templates. Existing workloads without
it fail the drift check; this fix does not authorize an automatic rollout or disk
migration. Journal writes advance to version 3 so older binaries reject the new
records instead of ignoring redistribution holds. Versions 1 and 2 remain readable;
downgrades after advancement are unsupported.

Hot-cell behavior and successful metrics collection remain tested with controlled
observations, not live load or a deployed Metrics Server. Kind has no Metrics
Server and verifies conservative missing-data behavior. No AWS/EKS/EBS,
contraction, upgrade, restart or completed deletion qualification is claimed.
CPU redistribution diagnostics do not prove application latency or durability.
Changes remain uncommitted for review.
