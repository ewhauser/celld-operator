# Step 1 findings — 18 September 2026

The pinned runtime passed the bounded local acknowledged-write checks below.
This is **not production qualification**. Repeated peer-disk recovery has positive
local evidence, but the candidate read sequence remains a hypothesis outside
these cases. Keep production automatic removal blocked in both modes until the
remaining gates pass; both scaling directions in both modes remain the intended
release scope.

## Verified candidate

- Release/commit: v0.5.0, `12d5b6333fe52717325addcfe1e99e9fd4f77bcd`.
  `git ls-remote https://github.com/denoland/celld.git refs/tags/v0.5.0`
  matched, as did the existing source checkout's clean HEAD.
- OCI index: `sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8`.
- Executed linux/arm64 manifest:
  `sha256:0c915aed95925945d145f242811b95cd4a6659ddd577dfbb6104c00c447b1e43`.
- linux/amd64 manifest, resolved but **not executed**:
  `sha256:3cb29128109213d30570c6503ccfd94cee1cd268be0e795d8cc04f448f6d91a8`.
- `docker buildx imagetools inspect` resolved the manifests; the executed image's
  OCI revision label matched the commit and `--version` reported 0.5.0.
  The harness checks the revision label on every run. No upstream files changed.
- Observed peer protocol 5 and bucket format 2. These are pin-specific private
  schemas, not a promise of compatibility with another release.

## Qualification matrix

Times include Docker/client overhead; recovery polls are approximately two
seconds apart. Single small runs are observations, not performance bounds.

| Case | Real local result | Qualification limit |
| --- | --- | --- |
| Fleet startup | Three nodes ready in about 1–4 seconds | No Kubernetes readiness/Service test |
| Two sequential graceful removals | Stops 0.214/0.234 s, exit 0; matching seals observed 28.519/30.444 s after stop; 60/60 acknowledged operations readable | Named volumes retained, processes stopped; no EBS or operator leader crash |
| Fleet graceful then SIGKILL | Stops 0.219/0.183 s, exits 0/137; matching seals 28.416/30.472 s later; 60/60 readable | No correlated host/disk loss; small sequential workload |
| Retained-disk restart | Same node `b`, same named volume, full 10 s TTL delay after confirmed stop; ready 1.063 s; 60/60 readable | New generation observed; old-generation evidence is not silently transferable |
| Fleet deadline-cut + storage stall | 1 ms configured shutdown budget, MinIO paused across termination; exit 0 in 0.149 s **with log still open**; seal 18.291 s later; 70/70 readable | Short local storage stall, not extended AWS outage; disk was retained |
| Bucket graceful, SIGKILL, restart, deadline | 70/70 acknowledged operations readable; exits 0/137/0; stopped records lacked a matching sealed log during each ~46 s window | Conservative recovery gate blocks absent log; this does not mean bucket writes were lost |
| Network partition | Isolated `a` from peers/store; process self-fenced with exit 3 after 10.466 s; seal 18.293 s later; 30/30 readable | Docker network partition, not EC2 termination or a stalled kernel; no conclusion from Pod disappearance |
| Pressured serving incumbent | Healthy incumbent becomes pressured under cgroup charge; joining health stays closed beyond a nonzero 3 s warning budget, clears after pressure removal | See pressure details below; no throughput or AZ claim |
| Historical loss declarations | Synthetic epoch and bundle keys block even with a sealed record, including later listing pages | No real runtime loss declaration induced |
| Parser/replay | Race-enabled parser tests and offline replay of real captures pass | Not proof of cloud consistency, IAM, or controller persistence |
| kind | **Not run** | No CRD/controller exists in this step |
| EKS / S3 / EBS / IAM / KMS | **Not run** | No account, cluster, bucket, or role supplied |

Sources: checked-in [fleet results](fleet/results.json),
[contraction results](contraction/results.json), [Bucket results](bucket/results.json),
[partition results](partition/results.json), and
[pressure with cold demand](pressure-demand/results.json). Each directory has the actual
node metadata, synthetic client ledger, and a provenance manifest. The
`before-abrupt`/`after-abrupt` filenames in the contraction run refer to the
second **SIGTERM**; `results.json` explicitly identifies `second-graceful`.

The pressure run uses a 64 MiB runtime memory threshold and a 96 MiB tmpfs charge
inside the incumbent's cgroup, after initial readiness and five acknowledged
writes. This exercises real runtime pressure without modifying celld. The incumbent initially became ready in 4.101 s, the joiner returned HTTP 503
on all 20 probes across the next 20 seconds, and became ready 1.013 s after the
charge was removed. All five acknowledged operations remained readable. The
joining gate is **not disabled**. A supplementary cold-read workload creates
admission demand. A joiner can host a cell through peer routing while its public
health is still 503: closed Service readiness does **not** establish that no
useful peer capacity appears. The measured incumbent backlog was one before join and zero after join; the
joiner had one resident cell while still unready. Sustainable demand relief and ready-only Service
routing remain separate release gates. Temporary cold demand is not a sustained
load benchmark.

## Adapter and safety boundary

`internal/runtime/v050` validates the image pin, required HTTP capacity fields and
freshness, node/key identity, protocol, exact generation (including the released
probe-key fallback), and folded log state. Missing/null capacity fields are
unknown, never zero. Duplicate JSON keys, malformed fields, unknown log states,
incomplete pages, repeated continuation tokens, access errors, and expired
assessments fail closed. Extra fields are opaque under the exact image pin.

The read-only `Reader` interface has only node GET and prefix LIST operations.
It is scoped to a primary fleet prefix by its eventual transport. `Assess`
checks the complete supplied session inventory, requires matching sealed records
for **all stopped sessions**, then scans the full historical `log/` listing for
both types of loss declaration. A live node's open log is not itself a blocker.
No log bodies or application objects are read by the collector. Generation
replacement, absent logs (including real Bucket-mode records), lost inventory,
and unresolved prior generations remain blocked. The adapter never performs
replication, recovery, lease writes, or disk removal.

An `Evidence` value is **candidate completion**, not a `SafeToRemove` decision.
The caller still owes a durable operation/session ledger, trustworthy process
fencing, membership and fresh metrics revalidation, lifecycle serialization,
and preservation of PVCs. The caller must persist loss findings and must not
forget unresolved sessions after restart. Those controller responsibilities
are deliberately outside step 1. The harness's confirmed Docker exits do not
implement Kubernetes fencing. Source ordering and local captures do not make
S3 reads across multiple keys atomic.

The Go adapter is replayed against the real captures with
`make qualification-replay`: five positive assessments and four expected blocks
(open logs after graceful/deadline exit 0, absent Bucket log, replaced generation). The Python
runtime harness collects real status and metadata and uses a narrower observation
check for polling; it does not run a controller. Its gate is recorded separately
from intentional subsequent **failure injections**, which may proceed even when
a controlled removal would be blocked. No automatic scaling is enabled.

Validation commands: `make check` passed (build, race tests, golangci-lint
v2.13.2: zero issues), including adapter cancellation and evidence validation
regressions. The two command packages currently have **no package tests**.
`make qualification-replay` passed its five positive/four negative cases.
The Python collector's eight fault/completeness tests passed with
`make qualification-test`. Python compilation and `git diff --check` passed.
The normal two-removal scenario was also repeated with the later harness;
[repeat results](contraction-repeat/results.json) retain the independent
measurement and readiness traces. A [final pressure repeat](pressure-repeat/results.json)
with the stricter listing collector again held all 20 joiner health probes at
503 and preserved 5/5 writes; readiness returned 1.011 s after relief.
All harness-created containers, volumes and networks were removed; the preexisting
kind container was left intact. Initial setup attempts failed before workload
execution (internal-network port publication; missing esbuild); these were fixed
in the checked-in harness. Pressure injection was refined from an already
unready low-memory process to a healthy serving incumbent before accepting the
reported pressure result.

Review added regression coverage and fixes for cancellation during the final
evidence read, listings without an explicit final page, pages after completion,
and cleanup failures that previously skipped resource removal. Negative replay
cases now require the expected blocking reason. CI runs offline replay and the
collector tests alongside the Go checks.

A fresh [review contraction run](review-contraction/results.json) against the
reviewed harness recovered all 60 acknowledged writes after two successive
removals. Recovery seals appeared after 28.406 s and 30.540 s; the final capture
also passed the Go offline assessment. All resources created by that run were
removed. The same limits on peer-only recovery proof apply to this run.

## Remaining release gates and exact AWS prerequisites

Supply an explicitly named nonproduction AWS account and region, EKS context and
namespace, externally provisioned worker capacity across the requested AZ count,
and a dedicated bucket/prefix with no lifecycle expiration/deletion of node or
loss evidence. Supply runtime storage credentials via the chosen EKS identity
mechanism, plus a **separate read-only operator role**: node-prefix GetObject and
prefix-restricted nodes/log ListBucket only. Supply KMS key and scoped decrypt
policy if applicable. Provide the installed EBS CSI driver, a supplied EBS
StorageClass, volume retention/reclaim settings, network-policy-capable CNI,
Metrics Server, and explicit permission for the proposed fault injections.
No default kubeconfig/cloud context is an acceptable substitute.

Then qualify, separately for each mode:

1. Real S3 pagination, conditional operations, error/staleness behavior and IAM
   denial of application/bundle bodies, unrelated prefixes, writes and deletes;
   capture a loss declaration produced by the actual runtime and recovery seal.
2. Repeated contraction with durable session history, operator crashes at every
   transition, prior unresolved sessions and generation replacement; no false
   success when node metadata vanishes. Define and qualify a Bucket no-log
   completion rule before enabling Bucket removal.
3. Acknowledged peer writes not yet in S3, simultaneous leader/follower loss,
   dormant cells, long S3 interruption, genuine EC2/node fencing, and retained EBS
   detach/reattach. The local runs did not isolate the winning proof for each ack;
   bucket uploads could win. Do not infer an RPO=0 peer-only guarantee from them.
4. Signal forwarding and enforced TTL restart spacing through container/pod/node
   failures (the harness's explicit sleep tests one restart, not a supervisor).
5. Sustained pressure/backlog scale-out with ClusterIP readiness routing, new-cell
   placement, handoff, rebalance and latency measurements. Qualify stabilization
   and thresholds rather than treating this small run as sizing guidance.
6. Strict configurable AZ placement and explicit relaxation, missing original
   EBS AZ, and follower diversity. Pod AZ spread still does not prove AZ-aware
   follower selection. Upgrades/rollback need their own version transition tests.

No AWS resource was created or modified, and no default cluster was used.

Step 2 follow-up: [local infrastructure and isolation evidence](infrastructure/README.md).
These checks add Kubernetes API, provisioning and network coverage; they do not
remove the production gates above.
