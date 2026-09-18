# Bucket scale-in and logical membership completion

18 September 2026. The operator implements **manual Bucket contraction** with the
unchanged pinned runtime and read-only `nodes/`/`log/` evidence. Automatic Bucket
contraction uses the same executor but remains release-gated in production until
EKS/S3 qualification. The fixed disposable MinIO configuration exercises automatic
execution locally with real Metrics Server; there is no production enable switch.
PersistentFleet contraction, upgrades and final deletion remain separate work.

## Adopted completion contract

A completed Bucket removal means the requested one-replica decrement has
converged in Kubernetes, every current survivor is ready and passes fresh runtime
and capacity checks, and every admitted generation outside current membership
has a positively observed expired lease with no peer-log recovery obligation.
The same membership must pass a second assessment after ten seconds of settling.
It **does not mean that every removed process is physically dead**.

`status.lifecycle.retiredBucketSessions` reports generations outside observed
membership whose physical liveness remains unknown. The retained journal records
`BucketMembershipConvergedProcessLivenessUnknown` as the completion outcome. Pod
absence and lease expiry are never labeled process-fencing certificates.

This deliberately revises the Bucket portion of ADR 0015's earlier stronger
contract. PersistentFleet retains its stronger
fencing and peer-recovery obligations because its acknowledged writes can still
exist only on peer disks.

## Why Bucket can use this contract

The pinned source always requires bucket durability for `CELLD_DURABILITY=bucket`,
including when peers exist ([main.rs](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L4213-L4220)).
Before releasing a bucket-proof response, the runtime reads cell ownership and
requires its node and epoch to match
([actor.rs](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/actor.rs#L3862-L3891)).
The upload precedes this read. A takeover before the read rejects the stale
acknowledgement; a takeover afterward restores a lineage already containing the
write. Superseded writers use their old epoch's objects. This is the basis for
acknowledged-write preservation, rather than physical process death.

A stopped response is ambiguous: an unacknowledged operation may have committed.
The operator never retries application writes or interprets a lost response as
proof that a write is absent. Clients remain responsible for retry semantics.

The contract assumes the pinned runtime, conforming object-store behavior,
exclusive fleet credentials/storage scope, and nonmalicious runtime and cluster
administration. It is not a Byzantine-writer guarantee. The Bucket path needs
observed node UID/generation/container consistency and verified runtime
configuration; it does not require the peer-authentication secret to certify
physical process identity. Unknown or replaced generations remain blockers.

## Durable execution

1. Persist a blocked request before any effect. The ordinary lifecycle authority,
   cancellation fence and 30-minute deadline still apply.
2. Inventory **every possible Deployment victim**, never a guessed deletion
   order. Validate Pod → ReplicaSet → Deployment UID ownership, pinned ReplicaSet
   template and actual Pod invocation/environment. Require exact current
   observational session associations. Unknown configuration, `envFrom`, duplicate
   variables and unqualified extra containers block.
3. Read complete node metadata and every `log/` listing page. Every generation
   must be admitted in current configuration or retained Bucket history. Current
   members require live leases and fresh samples; historical members absent from
   Kubernetes require positively read expired leases. Missing current or unresolved records, replaced records,
   unknown writers and any peer-log object block. Loss declarations acquire the
   existing sticky workload and reservation fence.
4. Project every possible donor's full observed demand onto every survivor.
   Read current Node identities/zone/hostname labels. Strict mode requires every possible victim to preserve all requested AZs, maxSkew 1 and hostname separation; Relaxed keeps the zone allowlist. Recheck the candidate set and placement around metadata and metrics collection. Persist the
   complete admitted session set and workload resourceVersion in `Intent`.
5. Repeat admission against that exact persisted set, recheck manual/automatic
   intent, and issue exactly one replica decrement through the existing workload
   CAS. A changed workload version is persisted before retrying; delayed issuers
   cannot decrement twice. Changed candidate sets remain blocked rather than
   silently retargeting the issued authority.
6. Reconstruct issuance after a crash from the matching workload operation ID
   and count. Wait for current Pod count/readiness, fresh survivor observations,
   and expired same-generation leases for members outside that set. A process
   isolated from peers but still renewing S3 keeps this phase pending.
7. Persist the first converged set, then repeat full assessment after ten seconds.
   Membership changes, lease renewal, missing metadata or unhealthy survivors
   restart settling. Deadline expiry never substitutes for completion; already
   issued operations continue assessment. Pause/deletion prevent new effects but
   preserve issued recovery authority.
8. Retain all admitted generations in `BucketHistory`, including logically
   retired ones. Subsequent additions receive fresh Pod UIDs. Later contractions
   recheck the full history, allowing repeated shrink/grow without discarding old
   evidence. Pinned `dead_node_gc.rs:399` deletes no-log dead records after a CAS tombstone. Once positive expiry and settling have been durably recorded, a later complete listing may omit that resolved generation. Absence cannot resolve a new retirement; unreadable listed records still block. Renewed historical leases block the next assessment. Reappearing
   retired identities and replaced generations are not adopted.

The journal writes version **5** and reads versions 1–5. Older binaries reject
version 5 instead of ignoring Bucket admission/history. Downgrade after advancement
is unsupported. History is never pruned to fit the existing 200 KiB journal budget.
Canceled unissued operations retain candidate admission records but never mark new historical sessions resolved.

## Automatic execution and local qualification

`Automatic` policy must still satisfy its complete/fresh low-demand history,
minimum samples, stabilization, bounds and cooldown, followed by all manual
admission checks and another current low-demand observation before issue.
Production automatic requests remain `BucketAutomaticUnqualified` until the AWS
release gate is closed in a reviewed release. Manual operation remains explicitly
experimental; it is not an assertion that AWS qualification has happened.

`--local-test --local-evidence` enables the same collector/executor against the
fixed `minio.celld-test-store.svc:9000` fixture and static public test credentials.
The second flag requires the first. It accepts no alternate endpoint or external
certificate and performs no AWS credential discovery. Without `--local-evidence`,
the existing native local harness remains free of S3 evidence requests.

The new runtime counter experiment separately exercises a paused owner during an
in-flight response, survivor takeover while that process still exists, resume,
and peer disconnection with continued S3 access. It checks acknowledged IDs and
unique sequence assignments, preserving ambiguous responses as ambiguous.
The operator integration runs in-cluster under its real ServiceAccount, with
NetworkPolicy, the unchanged runtime and Metrics Server. See the recorded
[validation results](qualification/bucket-lifecycle/README.md).

## Remaining release gates and limitations

- EKS/S3 identity, IAM isolation, KMS, pagination/faults and actual AWS storage
  behavior remain unqualified. No AWS account or default kubeconfig was used.
- Strict multi-AZ contraction can block when some Deployment victims violate spread: for example a 2/1 distribution cannot safely decrement with arbitrary victim selection, even though a particular victim would be safe. This needs a qualified victim-selection protocol or different workload layout to improve liveness; the operator never silently relaxes strictness.
- Broader workloads, clock skew/suspension timing, sustained concurrent writes,
  long-running connections, repeated cloud node partitions and soak tests remain.
- The operator conservatively blocks a lost/replaced historical node record,
  unknown prior writer, peer-log history, container generation replacement or a
  changed candidate set before issue. It offers no administrative success flag.
- A process retaining S3 access may continue renewing its lease indefinitely;
  the issued operation then remains pending. An infrastructure operator can
  restore networking or remove that process, but this operator has no EC2 fencing
  permissions and does not pretend logical expiry proves termination.
- A successful metadata scan is not an application-wide readback proof. The
  source-level acknowledgement rule provides the safety argument; local client
  ledgers are bounded counterexample tests, not a universal durability proof.
