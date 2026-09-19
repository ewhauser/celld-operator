# Requirements and code review before step 6

> Historical snapshot reviewed at commit `d3ea0df`. Its requirement table and
> findings describe that commit. The four P2 findings were fixed
> ([review corrections](qualification/review-fixes/README.md)); contraction,
> restarts, deletion and one runtime transition have since been implemented. The
> current matrix is [critical-features.md](critical-features.md).
Reviewed 18 September 2026 at `d3ea0df` on main. Baseline: the original
`Celld_Fleet_Operator_Design.docx` in Downloads, the user's accepted decisions
in ADRs 0002–0010, and the six-step implementation plan. Later implementation
ADRs describe the shipped behavior; they do not establish that all original
requirements have been delivered.

The four P2 findings below are the baseline review. Follow-up fixes and validation
are recorded in [review corrections](qualification/review-fixes/README.md).

## Assessment

The repository implements a conservative experimental fleet provisioner and
scale-out controller, with durable operation records, strict evidence parsing,
and explicit disruption blockers. It does not yet implement the complete
bidirectional autoscaler and lifecycle manager we planned. Steps 3 and 5 have
substantial scaffolding and negative-path coverage, but their principal
disruptive operations cannot execute in the shipped manager.

Failing closed is the right behavior given the evidence. However, missing
executors and production evidence providers are implementation work, not merely
release qualification. Step 6 cannot currently be treated as packaging alone.

## Actionable findings

### 1. Ineffective additions can keep consuming capacity

`internal/capacity/policy.go:158–176` recommends another batch whenever any
pod remains above a threshold. Its ineffective-capacity diagnostic at lines
124–134 only handles unready/unobserved replicas. A ready but idle new pod
clears pending state without showing that service improved.

Reproduced with the pure policy using defaults, three initial replicas, one
unchanging 400m CPU pod, and zero CPU on all others. Every new pod was ready
and freshly observed. Recommendations raised capacity to 4 at 30 seconds,
then 5, 6, 7, 8, 9 and 10 at five-minute intervals. Total CPU demand remained
400m. The configured maximum bounds spending, but does not implement the
original sections 8/13/15 hot-cell safeguard.

**P2:** Persist before/after demand and load distribution for additions. Detect
ready-but-unused capacity and stop repeated ineffective batches, with explicit
release conditions for the hold. Add the hot-cell acceptance test.

### 2. One slow fleet delays every other fleet

`internal/controller/reconciler.go:285–286` registers the controller without
concurrency options. The pinned controller-runtime v0.25.0 defaults to one
reconciliation worker. `capacityTarget` collects synchronously, and collection
has a ten-second deadline. Its eight workers parallelize pods inside one fleet,
not different fleets.

**P2:** Configure bounded cross-fleet reconciliation concurrency and test fairness
with a slow collector. For example, six fleets each exhausting collection's
deadline can take about a minute to revisit, exceeding the default 45-second
sample-gap limit and continually resetting stabilization. Pause and deletion
requests also share this queue. Original section 3 explicitly requires that one
fleet not block another. Sampling jitter is also absent.

### 3. Zone spreading does not enforce distinct Kubernetes nodes

`internal/controller/resources.go:93–107` sets zone node affinity and a single
zone topology-spread constraint. It has no hostname anti-affinity or hostname
spread rule. Multiple replicas may therefore share a node within an AZ; a
one-AZ fleet can put every replica on one node.

**P2:** Add and test the distinct-node placement required by original section 5,
with explicit relaxation semantics if desired. Zone distribution and follower
AZ diversity are separate issues; neither substitutes for node separation.

### 4. Blocked maintenance overwrites actual readiness with zero

`internal/controller/reconciler.go:254–275` assigns the supplied `ready` value
directly to status. Pause and blocked disruption paths supply zero without
observing that serving replicas became unready.

Reproduced through reconciliation: a Deployment reporting three ready replicas
continues reporting three after pause, while CelldFleet status changes from
three ready replicas to zero. Unsupported restart/image requests use the same
zero-reporting pattern.

**P2:** Report observed availability independently from permission to perform
lifecycle actions. Keep maintenance/disruption blockers separate from serving
readiness. This matters for both human diagnosis and availability alerts.

## Requirement coverage

| Requirement | Current behavior | Assessment |
| --- | --- | --- |
| Namespaced fleets, explicit durability, unchanged pinned celld | Go/controller-runtime, Bucket Deployment, PersistentFleet StatefulSet, pinned v0.5.0 | Implemented experimental foundation |
| Independent storage and private peers | Atomic whole-bucket reservation, fleet UID labels, direct Pod advertisements, NetworkPolicy, ClusterIP | Implemented with documented trust/CNI assumptions; prefix multiplexing intentionally omitted |
| Retained disks and stable identities | Retain policies, exclusive PVC creation, UID journal, stable persistent node name | Implemented locally; actual EBS restart/recovery remains unqualified |
| Configurable AZ count, strict default | Explicit AZ allowlist, strict/relaxed zone spread | Implemented; node separation missing; follower AZ diversity unproven |
| Manual additions | Journaled replica writes and exclusive PVC allocation | Implemented and previously exercised in disposable kind |
| Manual and automatic contraction in both modes | Bucket is always blocked; PersistentFleet execution requires an unexported synthetic test provider | Not delivered in the runnable operator |
| Live recovery evidence | Versioned parser and abstract read-only Reader; synthetic lifecycle evidence seam | Missing production S3 transport/IAM, session capture, survivor validation and fencing provider |
| Repeated shrink/grow lifecycle | Removed persistent identities are explicitly rejected on reactivation | Not supported even after the current synthetic contraction path; needs qualified retained-disk reuse |
| Optional Prometheus | Direct runtime and Metrics Server collection; controller metrics optional | Implemented architecture; successful live collection/automatic scale-out not demonstrated in the kind suite |
| Demand policy | Per-pod absolute CPU/memory/backlog thresholds, fixed batch, cooldown, complete coverage | Narrower than original aggregate CPU/residency sizing, deadband, separate pressure path and rolling-minute budget |
| Missing metrics and pending capacity | Fail closed, complete coverage required in both directions; any pending capacity blocks further additions | Conservative and tested; justified partial-data additions and independent-demand override omitted |
| Ineffective capacity | Detects prolonged unready pods only | Ready-but-idle additions missing; see finding 1 |
| Safe survivor capacity | Low per-pod thresholds plus injected validation in contraction tests | No shipped N-1 CPU/residency projection, donor analysis or production storage-health gate |
| Upgrades, rollback, planned restarts | Durable blocked requests; empty qualified transition matrix; no disruptive executor | Not delivered beyond request tracking and prevention |
| Maintenance pause | Pauses new work and fences unissued replica effects | Implemented; differs from original reduction-only maintenance with continued metrics and separate full pause |
| RetainData deletion | Finalizer and permanent reservation retained; no shutdown or cleanup executor | Data protection implemented; successful managed deletion not implemented |
| External HPA ownership | No External mode or /scale | Deferred original-design option; requires explicit scope decision before claiming full design coverage |
| Per-fleet resource/runtime tuning | Fixed 250m CPU request, no CPU limit, 512Mi memory request/1Gi limit; fixed shutdown/TTL | Proposed configurable resources, equal requests/limits, resident cap, idle eviction and lifecycle budgets absent |
| Network integration | Same-namespace labeled public clients; fixed HTTPS/DNS/peer/identity egress | Custom ingress-controller access and application egress need an external documented policy or API support |
| Explainable operation status | Capacity reasons, ready count, journal projection and broad conditions | Missing observed/applied/terminating counts, ages/timings, domain metrics/events; readiness defect above |
| Availability and recovery | Leader election; one operator replica in supplied manifest | Two-replica deployment and real leader failover qualification outstanding |
| Packaging and production qualification | Raw CRDs/manager/RBAC/examples; local harness | Helm, release pipeline and AWS failure suites remain step 6 |

## Validation and limits of this review

Fresh checks at the reviewed commit all passed:

- `make check`: build, Go race tests, golangci-lint (zero issues).
- `make manifests-check`: generated CRDs/deepcopy reproducibility.
- `make qualification-replay`: five positive candidate assessments and four
  expected blocks.
- `make qualification-test`: eight Python collector tests.
- Two temporary Go overlay reproductions: unchanged hot-pod demand repeatedly
  adds idle capacity; maintenance pause clears the observed ready count.

The overlay tests lived outside the repository and did not alter implementation
or checked-in tests. Review included the API/schema, controller startup and RBAC,
resource generation/comparison, reservation and PVC handling, lifecycle and
maintenance journal, capacity policy and collector, runtime evidence adapter,
and recorded qualification reports. No application code was changed.

Prior disposable-kind reports were inspected, not rerun for this read-only
review. They establish initial startup, manual additions, isolation and blocked
paths. They do not establish successful live automatic scale-out under load,
contraction, real node fencing, EBS recovery, upgrades or completed deletion.
No AWS or production checks ran in this review. Existing unit tests passing does
not supply the missing end-to-end acceptance evidence.

## Recommended work before a production step 6

1. Correct the four findings and explicitly settle the reduced API/policy scope.
   Keep whole-bucket isolation, direct metrics collection and conservative
   uncertainty handling; these are sensible documented choices.
2. Complete one real Bucket lifecycle path: live evidence collection, all-victim
   safety checks for Deployment contraction, survivor recovery/settling and
   acknowledged-write verification. Absence of a peer log is not by itself
   evidence that a Bucket process must be treated like a peer-log holder; define
   the mode-specific contract rather than permanently blocking that mode.
3. Implement and qualify PersistentFleet evidence, fencing, retained-EBS reuse,
   historical session accounting and repeated down/up cycles. Keep it disabled
   until that evidence exists.
4. Exercise the actual collector with Metrics Server and an in-cluster operator
   under sustained load, including hot cells, blocked joins, metrics loss and
   multi-fleet fairness. Measure response and recovery, not just recommendations.
5. Finish the shared restart/upgrade/deletion executor and qualify at least the
   explicitly supported transitions and final shutdown, or formally defer those
   product features. An empty transition matrix is safe but is not upgrade support.
6. Package and run the AWS release matrix against the resulting supported
   surface. Packaging an explicitly limited experimental build can proceed sooner;
   calling the original requirements complete cannot.
