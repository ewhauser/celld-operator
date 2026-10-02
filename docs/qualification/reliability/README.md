# Reliability fault injection

The reliability suite targets reconciliation boundaries, uncertain API outcomes
and recovery after observations disappear. The cases run against the current
StatefulSet-based lifecycle, in addition to the existing crash, drain, disk-loss
and whole-fleet Kind scenarios.

## Running it

```sh
make test-reliability
make test-envtest
make integration-partitions
make integration-faults
```

`make test-reliability` repeats the deterministic fault cases five times under
the race detector, with ordering seed `1`. Override `RELIABILITY_RUNS` and
`RELIABILITY_SEED` to reproduce another run. Every normal `make test` also runs
these cases once; every `make test-envtest` runs the real API cases. The
partition scenarios are included in `integration-faults`, so the existing
nightly fault job picks them up without a separate workflow.

## Fault matrix

| Layer | Injected faults | Required invariants |
| --- | --- | --- |
| Every traced API boundary in provisioning, drift correction, capacity persistence and deletion | Timeout, throttling, optimistic-lock conflict; successful write committed but response lost | Restart a new reconciler from persisted objects; converge within bounded retries; preserve reservation ownership, replica intent, concurrent metadata and storage identity; never claim readiness without workload-controller evidence. |
| Persistent member replacement and fleet cleanup | Lost claim repaired during deletion; Pod recovered or replaced after observation; init image/config failures; partial Pod/PVC delete with ambiguous response; missing, unknown or newly ready survivors | Retain disks when replacement evidence is stale or insufficient; resume an interrupted replacement after restart; replace only the intended disk. |
| Runtime and metrics observation | Connection reset, unavailable/denied endpoint, truncated or malformed body, oversized success/error response, stale/future timestamps, missing usage, cancellation | Reject incomplete evidence; never interpret failure as idle capacity; require new stabilization after recovery; cap body reads and close them. |
| Real kube-apiserver/etcd | Same-UID ownership edits, object name reuse, claim phase repair, concurrent status/spec edits, committed writes with lost responses | Enforce actual UID/resourceVersion conflicts; preserve another writer's intent; replay safely after restart. |
| Real Kind/Calico/MinIO/hostpath CSI | Operator observation partition, one member's peer RPC partition, fleet-wide S3 partition with manager restart | Prove new connections work before, fail during and work after the fault; preserve retained disk identities and every acknowledged write; observe operator coverage drop and recover. |

The API sweep discovers a successful trace and injects a fault at every call
index, rather than maintaining a hand-picked list of requests. A response-loss
case is included only when the backing write succeeded. Its subtest name includes
profile, phase, call index, verb, object and fault mode for exact reproduction.

## Bugs reproduced before fixes

| Bug | Failing evidence | Repair and regression |
| --- | --- | --- |
| Init-container image/configuration failure unnecessarily releases its retained disk | Each of the five recognized init waiting reasons deleted the down member's claim even though a new disk would not repair the init container. | Inspect both normal and init container statuses; `TestReliabilityPersistentInitConfigurationRetainsDisk`. |
| Repaired claim deleted after a stale `Lost` observation | Concurrent `Lost` to `Bound` update preserved UID; replacement still deleted the repaired claim with no conflict. | Pin PVC resourceVersion as well as UID; fake regression and `TestEnvtestReliabilityPersistentReplacementClaimChanged`. |
| Recovered Pod loses its disk after stale observation | A healthy same-Pod recovery or StatefulSet successor still caused claim deletion; replay subsequently deleted the recovered Pod. | Reread victim identity/version before committing claim deletion and pin Pod delete versions; `TestReliabilityPersistentRecoveredPodRetainsDisk` passes for recovery before the read. Recovery after the read remains a confirmed race, reproduced by the opt-in diagnostic below. |
| Fleet cleanup deletes a claim whose ownership changed | Concurrent fleet-label edit preserved UID; cleanup accepted deletion of the changed claim. | Pin cleanup claim resourceVersion; fake and real API cleanup ownership regressions. |
| Fleet deletion deletes a workload whose ownership changed | Concurrent fleet-label edit preserved UID; both fake and real APIs accepted UID-only deletion. | Pin workload resourceVersion; `TestReliabilityDeletionOwnershipCAS` and real API counterpart. |
| Metrics body budget is checked after buffering | A synthetic 8 MiB response was read in full before the existing 1 MiB size check. | The stream limit was subsequently merged in [PR #94](https://github.com/ewhauser/celld-operator/pull/94). This PR retains that implementation and adds `TestObservationReliabilityMetricsResponseBudget` coverage for success and error responses. |

## Evidence boundary

Fake clients do not implement all Kubernetes behavior, including UID delete
preconditions. The relevant identity and ownership races are therefore also run
against native kube-apiserver/etcd. Those tests have no kubelet, StatefulSet
controller, CSI backend or application process. Kind supplies those layers using
local hostpath CSI and MinIO; it does not qualify AWS EBS/EC2 or zone failure.

Pod reread, PVC deletion and Pod deletion remain separate API operations. The
replacement change rejects recovery already visible at its final Pod read;
it does not make recovery and replacement atomic across objects. The opt-in
diagnostics below confirm that later recovery still races the PVC mutation:
claim deletion commits, stale Pod deletion conflicts, and the next reconcile
deletes the healthy Pod. A stronger guarantee requires a separately designed
replacement protocol. These diagnostics intentionally fail on the current patch;
normal checks skip them explicitly. The fake case includes replacement selection;
the real API case isolates an already-selected replacement, then exercises the
next healing reconcile. It confirms an actual PVC deletion timestamp and an
actual Pod resourceVersion conflict, without a kubelet or StatefulSet controller.

```sh
CELLD_TEST_REPLACEMENT_RACE=1 go test -race ./internal/controller \
  -run '^TestDiagnosticPersistentReplacementRecoveryAfterFinalRead$' -count=1 -v

# With KUBEBUILDER_ASSETS pointing to envtest's API server and etcd binaries:
CELLD_TEST_REPLACEMENT_RACE=1 go test -race ./internal/controller \
  -run '^TestEnvtestDiagnosticPersistentReplacementRecoveryAfterFinalRead$' -count=1 -v
```

The live partitions last at least 30 seconds after denial is established,
shorter than the member replacement delay. Calico denial is proved with new TCP
connections; it does not establish that every existing RPC stream was severed.
The observation fault restarts the manager under the active deny to close its
pre-existing keep-alive sessions. Its Shadow capacity view must report two useful
and covered members with `PendingCapacity`, then recover to three.
Every forced manager restart requires a target Pod and a Ready successor with
a different UID; Deployment rollout status alone cannot establish replacement.
The peer test directly proves inbound denial; its reverse-direction denial is
configured by the same policy. S3 data is preserved during the outage. Loss of
the last replicated copy, prolonged partitions and cloud-provider detach remain
outside this evidence.

## Local run, October 1, 2026

This run tested the original reliability patch on operator `8a2a69b`.
The live runtime was `v0.6.0-ewhauser.2`, pinned to
`ghcr.io/ewhauser/celld@sha256:94f35ba6973942eb4aaad2830ee8ebe431e2a125940acf380e74782602980a2a`.
The local run used macOS arm64 Go 1.27.1, native
Kubernetes 1.37 envtest, and a separate disposable Colima VM with three
Kind Kubernetes 1.31.4 nodes. The passing regression matrix adds 617 ordinary
test cases, including 546 API injections (234 timeouts, 234 throttles, 48
committed writes with lost responses, 30 conflicts), 47 observation/recovery
cases, 21 persistent-member faults and complementary membership/deletion/probe
oracles. The real API layer adds 27 cases.

| Verification | Result |
| --- | --- |
| Full `go test -race -timeout=30m ./...` | Passed, 1,090 active leaf cases; controller package 216.5 seconds. Envtest requires its separate asset-enabled run; the known diagnostics skip unless opted in. |
| Full asset-enabled `TestEnvtest*` run with `-race` | Passed, 70 active leaf cases including 27 new reliability cases; 16.7 seconds. Optional Gateway-schema case skipped because no Gateway CRD was supplied. |
| Earlier repeated fault sweep, seed 1, five runs | Passed, 561.8 seconds. |
| Final repeated fault sweep, seed 42, two runs | Passed, 233.0 seconds. |
| Final Kind observation, peer and S3 partitions | Passed. Observation coverage/useful members went 3 → 2 (`PendingCapacity`) → 3; each manager kill produced a new Ready Pod UID. Every disk retained its PVC/PV/CSI identity. The ledger was readable after each fault: 135/135, 206/206, then 283/283 acknowledged writes. |
| Build, native/Linux lint, generated-manifest and chart-source consistency | Passed. |
| Opt-in replacement race diagnostics, fake client and real kube-apiserver | Both failed as intended on the desired safety invariant: PVC terminating, first Pod delete conflicted, subsequent reconciliation deleted the recovered Pod. |

Raw JSON test results, live cluster output and diagnostic logs are saved locally
in the ignored `.qualification-runs/reliability-2026-10-01/` directory. The live
harness deleted its own cluster; the separate test VM was removed after the run.

## PR validation, October 2, 2026

The PR is rebased onto `fcc3246`, including the subsequent mesh, export,
runtime-pin and performance changes. The metrics stream limit already merged
in PR #94 is retained unchanged; the new budget regressions exercise it.
The API sweep discovers the rebased controller's requests at runtime, so
the October 1 injection count is a receipt for that earlier source state.

`make check` passed on the rebased source: build, full race tests (controller
193.4 seconds), native lint and Linux lint. The asset-enabled full envtest suite
also passed: 79 active cases including 27 reliability cases, in 16.5 seconds.
The opt-in diagnostics continue to reproduce the unresolved replacement race.

The October 1 Kind and repeated-sweep receipts above retain their original
source/runtime boundary. Kind was not rerun after this rebase or the main-branch
pin to `v0.6.1-ewhauser.1`. Rebased results are saved beside the original receipts
as `pr-make-check.log`, `pr-envtest.jsonl` and `pr-known-replacement-race.log`.
