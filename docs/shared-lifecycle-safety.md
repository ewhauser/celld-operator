# Shared lifecycle safety implementation

18 September 2026. This implements shared prerequisites, not either profile's
removal executor or production qualification. Both Bucket and PersistentFleet
remain in scope, and both remain unable to contract in the runnable operator.

## Production collection and authority

The normal manager now installs `ProductionEvidence`. It reads the reserved
primary bucket with the operator identity, independently of the runtime's writer
identity. The local disposable manager deliberately does not install an AWS
reader. There is no production-enable or fencing-attestation boolean.

The AWS SDK v2 transport signs GetObject only for `nodes/<identity>.json` and
ListObjectsV2 only for `nodes/` and `log/`. It has no mutation methods, no reads of
log bodies, no alternate bucket/prefix API, no redirect following, a three-second
request limit and two SDK attempts within that limit. Inventory has a five-second
budget and the pod/S3 observation has an eight-second budget. Cancellation reaches
credential retrieval, requests and body reads. Missing objects, denial, malformed
or partial bodies, missing pagination flags, repeated tokens, duplicate keys and
budget exhaustion provide no positive evidence. Limits are 1 MiB per node body,
1,000 pages and 100,000 listing keys; exhausting them blocks assessment. Loss
names are checked after node records. A loss found on a partial scan is already
negative evidence; a later failed page cannot turn it into success.

Credential integration uses the AWS SDK's environment/web-identity/container
providers, including IRSA and EKS Pod Identity. Shared credential/config files and
EC2 instance metadata fallback are disabled. The operator must receive a separate
externally provisioned role. No AWS request or credential lookup was used to
validate this implementation; SDK wire tests use a local HTTP server and dummy
credentials. IAM isolation, KMS and live EKS authentication remain unqualified.
See [SDK configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html).

Persisted inventory is separate from a permission to disrupt. Every observed
runtime generation and recovery epoch is retained, with the observed pod UID,
container ID/restart count, Kubernetes host name, timestamps and association
quality where available. Binding observations require matching node naming and
Pod IP advertisement, a lease load sample after container start, and unchanged
container identity around the metadata reads. Bucket node names are pod UIDs;
PersistentFleet names are StatefulSet pod names. Unknown writers, missing records,
replacement generations and ambiguous associations are retained. Partial scans
clear current/freshness indicators, never historical records. A newer epoch of
one unchanged observed process does not erase the earlier epoch. Replaced
processes remain unresolved; no age limit automatically forgets them.

The reservation journal now reads versions 1/2/3/4 and writes version 4. The
inherited P2 version-3 reader and redistribution state are preserved on upgrade;
version 4 prevents an older binary from ignoring cancellation/deadline/session
state. Downgrade after advancement is unsupported. A 200 KiB journal budget
fails before new actions instead of pruning safety history. Status exposes
session count, evidence blocker/freshness, operation deadline and stalled state.
Status is never authority. Loss remains sticky on the workload CAS object and
reservation; repeated loss reports do not starve compatible additions.

## Exact identity and fencing boundary

`ObservedUnverified` explicitly does **not** prove a cryptographic runtime-to-
container binding. The pinned `/state` does not expose the runtime generation.
The existing signed `/peer/probe` can prove possession of that generation's key,
but requires the fleet peer-authentication secret, outside the accepted
`nodes/`/`log/` metadata scope. The operator does not read that secret, widen IAM,
or pretend address matching is an authenticated probe. See the pinned
[probe handler](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L2577-L2619)
and [probe protocol](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/peer_probe.rs).

The process fencing contract requires independently verified, irreversible
termination of the exact pod UID/container invocation and prevention of that
invocation's restart, or fencing of the exact host incarnation plus a guarantee
that it cannot resume storage/network access. Evidence must bind fleet,
operation, runtime generation, container identity and infrastructure identity;
a replacement host or pod name cannot inherit it. Recovery still requires the
matching epoch, seal and complete subsequent loss scan. Process fencing is not
recovery completion.

No current production provider satisfies that contract. Pod absence, force
removal, deletion timestamp, readiness changes, NodeReady, successful exit,
expired leases and sealed live logs are explicitly unsupported evidence.
Kubernetes API state cannot rule out a partitioned process continuing to run.
The production `Stopped` path always returns `FencingUnqualified`; the synthetic
in-package provider is still available only to tests. No EC2 control privilege,
external certificate acceptance or administrative bypass has been introduced.

The precise external dependencies are (1) a qualified trusted association of
runtime generation to container invocation within the narrow access boundary,
and (2) a qualified host/process fencing authority with exact infrastructure
identity and restart prevention. An EKS infrastructure integration that can
establish and verify irreversible instance termination is one candidate; its
contract, permissions and AWS failure tests are not supplied here. An expanded
peer-authentication scope would require a separate decision and would solve
identity only, not fencing. These limitations are visible implementation
blockers, not a claim that tests have qualified contraction.

## Health and capacity preflight

Production blocked removal requests execute the shared survivor preflight using
the direct runtime/metrics collector, even for manual requests (default capacity
thresholds apply when no policy is supplied). It requires complete, fresh,
healthy exact-container observations, no drain/rebalance/recovery backlog,
readiness and memory headroom. The donor's full observed CPU and memory demand
(using the larger of container memory and runtime RSS/in-use memory)
must fit on **each** possible survivor below the high thresholds. This deliberately
avoids assuming even redistribution. Generation replacement, missing samples,
pressure and insufficient projected capacity block. These are observational
capacity bounds, not an application performance or future-load guarantee.
The residency/resource sizing policy remains a separate design gap; thresholds
do not prove per-cell working-set behavior or storage durability. Neither this
check nor a healthy fleet supplies a process fence.

## Deadlines, restart and compatible additions

Each new operation has a persisted start and a 30-minute deadline. Legacy active
operations receive a single persisted deadline on first resumption. Deadline
expiry marks stalled state and prevents an unissued replica effect, including
one attempted by a delayed issuer. Already issued effects remain authoritative
and can be reconstructed/completed after expiry. Issued removals keep polling
recovery; a timeout never substitutes for fencing or recovery completion.
Request cancellation also propagates before any journal/replica write.

An unsupported production removal is persisted in `Blocked`, without inventing
a qualified target capture. It cannot transition into execution. New desired
additions (manual or a fresh actionable capacity decision) can supersede a
blocked/unissued removal through `Canceling`:

1. Persist cancellation intent on the reservation.
2. CAS a cancellation marker onto the workload, invalidating delayed old replica
   writes on that same object. This marker is a replica-authority fence, **not**
   a process fence.
3. If removal issuance won first, reconstruct `Recovering` and retain the old
   authority. Otherwise archive `CanceledBeforeIssue` and clear the operation.
4. Re-evaluate the latest request, then journal and execute the normal addition.

Crashes after any of these steps replay safely. Reversed intent never retargets
an operation in place. Existing pause/delete fences take precedence. Additions
use fresh ordinals and exclusively created PVCs. An issued PersistentFleet
removal cannot be bypassed: the next ordinal would reuse its retained identity,
and disk reactivation is a separate checklist item. Bucket additions alongside
an issued removal also remain queued until its separate executor establishes
its membership/recovery contract. Unsupported upgrades, restart and deletion
remain separate durable requests.

## Remaining qualification

Read-only IAM enforcement (including denied application/bundle reads and writes),
live EKS identity, S3/KMS faults, actual Metrics Server load, process/node fencing,
mode-specific shutdown and repeated contraction, retained-EBS reactivation,
acknowledged-write preservation, upgrades and deletion are not qualified.
See [local validation](qualification/shared-lifecycle/README.md). Whole-bucket
isolation, external worker/bucket/IAM/ingress provisioning, strict configurable
placement, unmodified image pin and optional Prometheus are unchanged.

Bucket-specific follow-up: [all-candidate admission and completion boundary](bucket-scale-in.md)
separates the Bucket acknowledgement rule from peer-log recovery without weakening
the process-completion contract above. Its metadata observation does not enable
contraction.
