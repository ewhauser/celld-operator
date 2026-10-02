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
make integration-replacement
make integration-faults
```

`make test-reliability` repeats the deterministic fault cases five times under
the race detector, with ordering seed `1`. Override `RELIABILITY_RUNS` and
`RELIABILITY_SEED` to reproduce another run. Every normal `make test` also runs
these cases once; every `make test-envtest` runs the real API cases. The
partition and focused replacement scenarios are included in `integration-faults`,
so the existing nightly fault job picks them up without a separate workflow.

## Fault matrix

| Layer | Injected faults | Required invariants |
| --- | --- | --- |
| Every traced API boundary in provisioning, drift correction, capacity persistence and deletion | Timeout, throttling, optimistic-lock conflict; successful write committed but response lost | Restart a new reconciler from persisted objects; converge within bounded retries; preserve reservation ownership, replica intent, concurrent metadata and storage identity; never claim readiness without workload-controller evidence. |
| Persistent member replacement and fleet cleanup | Lost claim repaired during deletion; Pod recovered or replaced after observation; init image/config failures; prepared or committed handoff interrupted; writes with ambiguous responses; missing, unknown or newly ready survivors | Retain disks when replacement evidence is stale or insufficient; abort a prepared handoff after restart; resume a committed one; hold the old Pod name until its original claim deletion is accepted; preserve other finalizers. |
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
| Recovered Pod loses its disk after stale observation | Even after a final Pod reread, later same-Pod recovery allowed PVC deletion to commit; Pod deletion conflicted, then replay deleted the recovered Pod. Fake and real API diagnostics reproduced this order. | Commit an acknowledged UID/resourceVersion-bound Pod delete before touching its claim, hold that Pod's name with a finalizer, and pin the original PVC UID/resourceVersion. `TestReliabilityPersistentRecoveryBeforePodDeletion` and `TestEnvtestReliabilityReplacementRecoveryBeforePodDelete` turn those diagnostics into ordinary regressions; handoff replay cases cover uncertain responses and restart. |
| Fleet cleanup deletes a claim whose ownership changed | Concurrent fleet-label edit preserved UID; cleanup accepted deletion of the changed claim. | Pin cleanup claim resourceVersion; fake and real API cleanup ownership regressions. |
| Fleet deletion deletes a workload whose ownership changed | Concurrent fleet-label edit preserved UID; both fake and real APIs accepted UID-only deletion. | Pin workload resourceVersion; `TestReliabilityDeletionOwnershipCAS` and real API counterpart. |
| Metrics body budget is checked after buffering | A synthetic 8 MiB response was read in full before the existing 1 MiB size check. | The stream limit was subsequently merged in [PR #94](https://github.com/ewhauser/celld-operator/pull/94). This PR retains that implementation and adds `TestObservationReliabilityMetricsResponseBudget` coverage for success and error responses. |

## Evidence boundary

Fake clients do not implement all Kubernetes behavior, including UID delete
preconditions. The relevant identity and ownership races are therefore also run
against native kube-apiserver/etcd. Those tests have no kubelet, StatefulSet
controller, CSI backend or application process. Kind supplies those layers using
local hostpath CSI and MinIO; it does not qualify AWS EBS/EC2 or zone failure.

Pod and PVC mutations remain separate API operations. The replacement protocol
uses an acknowledged Pod delete, with UID and resourceVersion preconditions, as
its commitment point. Before that delete, the operator prepares its own finalizer
and a record of the fleet UID and original PVC UID/resourceVersion on the
selected Pod. A recovery before the delete conflicts and keeps the disk. After it commits, the
terminating Pod and finalizer hold the StatefulSet name so a successor cannot
reattach the old claim while deletion is being decided.

Only an acknowledged delete can persist a committed record. A new reconciler
aborts a prepared record and keeps the claim, even when the Pod delete succeeded
but its response was lost or another actor deleted the Pod. A committed record
resumes against the original claim UID/resourceVersion. A changed live claim is
retained; a new claim with the old name is never deleted. The operator releases
its own hold once the original claim's deletion is durably accepted or its UID
is absent, rather than waiting for Kubernetes' PVC protection to finish. It
preserves unrelated Pod finalizers.

The handoff cleanup runs before runtime validation and workload gates and scans
Pods indexed by the private `celld.eric.dev/replacement-fleet` label, even outside
the requested replica range. The persisted fleet UID and private index let the
operator abort and release its hold after ordinary fleet-label or owner-reference
drift; that drift cannot authorize disk deletion. Finishing or aborting removes
the operator's record, index label and finalizer together. Pausing or deleting a
fleet releases its holds without starting another disk deletion. An already accepted
deletion remains irreversible. The regressions exercise these API boundaries;
envtest establishes the real precondition and finalizer behavior, while a live
StatefulSet/CSI run is needed to establish PVC-protection completion and fresh
claim provisioning. `make integration-replacement` runs that focused lost-volume
scenario under continuous writes, requires a fresh Pod/PVC/PV/CSI identity for
the victim, retained identities for every other disk, and no leaked replacement
record, index label or finalizer. Typed Pod/PVC watches retain the API transitions
and report which short handoff phases were captured. A Pod read after the original PVC's
deletion timestamp can prove that the old Pod's hold still exists then; a missed
sample is reported explicitly and does not establish that phase. This live test
does not inject a manager restart at a deterministic handoff boundary; fake and
real API replay tests target each write boundary instead.

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
| Full `go test -race -timeout=30m ./...` | Passed, 1,090 active leaf cases; controller package 216.5 seconds. Envtest requires its separate asset-enabled run; the then-known diagnostics skipped unless opted in. |
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

Reliability PR #95 was rebased onto `fcc3246`, including the subsequent mesh, export,
runtime-pin and performance changes. The metrics stream limit already merged
in PR #94 is retained unchanged; the new budget regressions exercise it.
The API sweep discovers the rebased controller's requests at runtime, so
the October 1 injection count is a receipt for that earlier source state.

Before the replacement handoff fix, `make check` passed on the rebased source:
build, full race tests (controller 193.4 seconds), native lint and Linux lint.
The asset-enabled full envtest suite
also passed: 79 active cases including 27 reliability cases, in 16.5 seconds.
The opt-in diagnostics still reproduced the late-recovery replacement race on
that source state. The ordinary regression suite now includes that failure order
and the replacement handoff protocol described above.

The October 1 Kind and repeated-sweep receipts above retain their original
source/runtime boundary. This initial PR validation did not rerun Kind after
the rebase or the main-branch pin to `v0.6.1-ewhauser.1`. Rebased results are saved beside the original receipts
as `pr-make-check.log`, `pr-envtest.jsonl` and `pr-known-replacement-race.log`.

## Replacement fix validation, October 2, 2026

The follow-up fixes the remaining replacement race after PR #95 merged. Its
baseline is `7cfd09d`, whose source tree matches the previously tested `1e66686`.
This run uses the handoff implementation and regressions described above,
macOS arm64 Go 1.27.1, native Kubernetes 1.37 envtest, and a new disposable
Colima VM with three Kind Kubernetes 1.31.4 nodes. The live runtime is
`v0.6.1-ewhauser.1`, pinned to
`ghcr.io/ewhauser/celld@sha256:e8dc139226269acf137b54b67cea42a3b6a7c6527ce968caa5d303c21c3b7b74`.

| Verification | Result |
| --- | --- |
| `make check` | Passed: build, full race tests (controller 230.0 seconds), native lint and Linux lint. |
| Full asset-enabled `TestEnvtest*` run with `-race` | Passed: 98 active leaf cases, including 46 reliability cases and 19 new replacement protocol cases; 24.4 seconds. The existing Gateway-schema case skipped because no Gateway CRD was supplied. |
| Persistent reliability cases repeated three times with `-race` | Passed; 81.5 seconds. Includes interrupted writes, recovery, ownership changes, concurrent aborts and cleanup gates. |
| Generated manifests and `make chart-check` | Passed: canonical CRD/RBAC consistency, Helm lint/rendering and release checks. |
| Focused Kind replacement suite | Passed: the victim received new Pod/PVC/PV/CSI identities; every other disk retained its identity; PVC protection completed; no replacement annotation, index label or finalizer leaked. At most one member was down. All 36/36 acknowledged writes remained readable, including 24 writes acknowledged during the fault. |
| Live handoff evidence | Pod/PVC watches captured preparation, a committed terminating Pod, old PVC deletion and both successors. A fresh Pod read after the old PVC deletion event still found the committed hold. The final trace reported all three phases captured. |

The Kind suite removes the original PV and its finalizers to simulate a cluster
reporting a lost volume. It qualifies StatefulSet/PVC-protection/hostpath-CSI
completion, rather than physical cloud-disk loss or AWS detach. It does not
inject a deterministic live manager crash during the handoff; the fake and real
API tests inject interruption and response loss at each of its five writes.

Evidence is saved locally beside the earlier receipts as
`replacement-final-check.log`, `replacement-final-envtest.json`,
`replacement-repeat.log` and `replacement-kind.log`. The harness removed its
own cluster, and the test VM was deleted afterward. The user's default Docker
context and existing Colima VM remained unchanged.
