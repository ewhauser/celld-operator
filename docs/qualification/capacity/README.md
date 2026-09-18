# Step 4 capacity policy validation

18 September 2026. Worktree `/Users/ewhauser/.codex/worktrees/a90b/celld-operator`.
Step 2 `a4b9edb` and step 3 `486cae6` were cherry-picked as `aa02ef7` and
`225fc33`. `git diff 486cae6 HEAD --exit-code` confirmed identical baseline
content before the new changes. Main remains `83d1627`. New implementation
changes are uncommitted; nothing was pushed or submitted as a PR.

The new policy and collector support both existing profiles. See
[ADR 0013](../../decisions/0013-capacity-policy.md) and the
[configuration guide](../../capacity-policy.md). The shipped contraction gates
are unchanged: Bucket has no qualified completion/victim rule and PersistentFleet
has no production process fence or AWS recovery transport. The local injected
qualification seam remains inaccessible through flags or CR fields.

## Unit and integration coverage

The pure policy tests exercise bursts, upper/lower bounds, per-node CPU/memory,
runtime pressure/backlog, declining demand, full stabilization windows, cooldowns,
missing/partial/stale/future/replayed samples, missing Pods, duplicate identities,
measurement-window limits, clock rewind, collection gaps, policy/membership edits,
serialized restart history and unready joiners exceeding the provisioning timeout.
Frequent reconcile tests preserve status diagnostics without making a cached
recommendation executable.

HTTP collector tests use a real local HTTP test server and the actual Kubernetes
REST client. They cover private `/state` and the metrics.k8s.io PodMetrics path,
required fields/quantities, source and receipt clocks, resource averaging windows,
403s, cancellation, oversize runtime responses, redirect rejection, unknown state,
unready Pods and container replacement during collection. This validates transport
and interpretation against controlled responses, not a deployed Metrics Server.

Controller tests use Kubernetes fake clients with optimistic resource versions
and race detection. Both profiles exercise shadow workload immutability, explicit
automatic additions through the existing journal, pending/ineffective capacity,
controller reconstruction, manual override persistence, policy bounds/mode changes
during an operation, simultaneous reconciles, disk identity blockers, edits during
collection and the unchanged production contraction gates. The private synthetic
PersistentFleet seam additionally verifies low-demand revalidation and policy/loss
blockers before removal, including re-stabilization after pressure invalidates an
already recorded removal window. Synthetic evidence does not qualify live contraction.

## Recorded commands

All commands below passed on the final implementation. The full lint run reports
zero issues. Python compilation and `git diff --check` also passed.

- `make check`: build, all Go race tests and golangci-lint v2.13.2 with Go 1.27.1.
  [Log](check.log).
- `make manifests-check qualification-replay qualification-test`: generated
  CRD/deepcopy reproducibility; five positive offline candidate assessments and
  four expected recovery blocks; all eight Python qualification tests.
  [Log](qualification.log).
- `go test -race ./internal/controller ./internal/capacity ./api/v1alpha1 -run
  'Capacity|Collector|Policy|LowDemand|Burst|Declining|Uncertainty|Replay|Deterministic|Frequent'
  -count=1 -v`: [focused log](focused-tests.log).
- `python3 hack/integration/run.py`: all assertions passed in disposable cluster
  `celld-step2-4118ffcf`; exit code 0. [Disposable kind log](kind.log).
  The cluster and native operator process were removed; a final Docker/process
  check confirmed none of this task's three clusters or operator processes remain.

The first disposable run (`celld-step2-d40f50b7`) exposed flickering capacity status
between observation intervals. It was interrupted after diagnosis and its cluster
and operator were cleaned up. [Original log](initial-kind.log). A regression now
keeps diagnostics stable while disallowing cached recommendations from acting.

A second run (`celld-step2-c3858f34`) failed before operator assertions because
the seed job raced MinIO listener startup. Its cluster was cleaned up.
[Setup failure log](seed-retry-kind.log). The seed command now retries startup
for at most 30 attempts before failing; it does not retry operator assertions.

## Limits

The local kind suite uses externally supplied local MinIO, local-path PVCs and the
pinned unchanged celld v0.5.0 image with actual manager RBAC. It verifies existing
runtime startup, same-fleet/operator isolation, strict missing-AZ scheduling,
restart-safe manual additions, retained disks and unchanged contraction gates.
The added API checks cover capacity defaults, invalid bounds, mutable policies,
shadow mode and conservative automatic behavior with Metrics Server absent.
The native operator cannot establish the EKS Pod-network/metrics aggregation path;
successful collection and pressure decisions here are tested with HTTP/fake-client
fixtures. No live automatic addition under sustained load is claimed.

No AWS resources or pre-existing/default clusters are used. Only invocation-owned
clusters and operator processes are cleaned up; Docker images/build caches remain.
There is no AWS/EBS/IAM, production durability, real-process fencing, successful
pressure-relief, follower AZ-diversity, upgrade, maintenance or release qualification.
CPU/memory observations and Kubernetes readiness do not establish recovery safety.

## Review fix: intervening observations

Regression tests first reproduced premature recommendations in both directions
when observations inside the sampling interval contradicted the existing window.
The evaluator now checks completeness, freshness, readiness and demand before
applying the interval gate. Intervening negative evidence resets stabilization;
supporting observations do not advance counts, accepted timestamps or source
watermarks. Stable diagnostics remain visible without cached actions executing.

The new tests cover opposite/neutral demand, missing metrics, partial inventory,
unready replicas and stale runtime observations in both directions, recovery
after a fresh stable window, and frequent supporting samples.
`go test -race ./internal/capacity ./internal/controller` passed.
The final `make check` result is recorded in [review-fix-check.log](review-fix-check.log).
The earlier kind run was not repeated for this pure policy change.
