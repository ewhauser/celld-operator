# Experimental fleet provisioning

The operator provisions Bucket and PersistentFleet workloads and coordinates
manual scaling, maintenance, and retained-data deletion. The
[current capability matrix](critical-features.md) distinguishes implemented paths
from unsupported operations and outstanding cloud testing. For installation,
follow the [user guide](../site/src/content/docs/start/install.mdx).

Use Kubernetes 1.31 or newer with IPv4 Pod networking for this prototype. Before installation, externally provide:

- A verified NetworkPolicy-capable CNI and capacity in the chosen AZs.
- One dedicated S3 bucket per fleet, with no external writers or evidence expiry.
- A ServiceAccount in each fleet namespace, wired to an externally managed runtime
  IAM role (EKS Pod Identity or qualified IRSA). No credentials are stored in the CR.
- For PersistentFleet, EBS CSI and a StorageClass using `ebs.csi.aws.com`,
  `reclaimPolicy: Retain`, and `volumeBindingMode: WaitForFirstConsumer`.

Use an **explicit context** when installing. Build and load/publish the operator
image yourself; the development manifest references an unpublished `:dev` image.
The operator requires a separate narrowly scoped read-only AWS identity for `nodes/`/`log/` evidence. It writes no S3 metadata and creates no AWS resources itself. On EKS, externally installed CSI provisions volumes for requested PVCs.

```sh
kubectl --context YOUR_EXPLICIT_CONTEXT apply -f config/crd/
kubectl --context YOUR_EXPLICIT_CONTEXT apply -f config/manager/operator.yaml
# Grant the namespaced fleet Role in every namespace that will hold fleets.
kubectl --context YOUR_EXPLICIT_CONTEXT -n YOUR_FLEET_NAMESPACE apply -f config/rbac/fleet-namespace.yaml
# Only after verifying CNI enforcement, add --network-policy-enforced to operator args.
# Customize the examples' namespaces, ServiceAccounts, buckets, region, AZs and class.
kubectl --context YOUR_EXPLICIT_CONTEXT apply -f config/samples/bucket.yaml
kubectl --context YOUR_EXPLICIT_CONTEXT apply -f config/samples/persistent.yaml
```

Samples are not immediately deployable infrastructure: replace their bucket names
and provision the referenced ServiceAccounts. The existing workload identity must
have the runtime's necessary bucket permissions; the production evidence reader needs a separate read-only identity. See [shared lifecycle safety](shared-lifecycle-safety.md).

| Field | Meaning |
| --- | --- |
| `qualification` | Required literal `Experimental`; no production setting |
| `profile` | Required `Bucket` or `PersistentFleet` |
| `replicas` | Manual target/override, 1–100; default 3; at least AZ count |
| `runtimeImage` | Requested celld release digest; see [runtime versions](runtime-versions.md) for accepted images and the one-way PersistentFleet upgrade |
| `maintenance.paused` | False by default; suspend new and unissued actions, continue issued recovery |
| `maintenance.restartToken` | Change the token to request a same-version restart; placement and shutdown checks must pass |
| `capacity` | Optional policy: `Shadow`, `ScaleOut`, `Automatic`, or `External` (one `/scale` writer owns `spec.replicas`); see [capacity policy](capacity-policy.md) |
| `serviceAccountName` | Existing account in the fleet namespace |
| `storage.bucket` | Dedicated canonical bucket, lowercase letters/digits/hyphens |
| `storage.region` | Explicit AWS region; immutable |
| `storage.sizeGiB` | Disk limit/PVC size, default 10 GiB |
| `storage.storageClassName` | Required only for PersistentFleet |
| `placement.zones` | Explicit distinct standard AZ names in the storage region |
| `placement.azCount` | Must equal number of zones, 1–6 |
| `placement.mode` | Strict (default) or Relaxed; same allowlist in either mode |
| `execution.cpuRequest` / `cpuLimit` | celld container CPU; defaults 250m and no limit; limit ≥ request; immutable |
| `execution.memoryRequest` / `memoryLimit` | celld container memory; defaults 512Mi and 1Gi; limit ≥ request; immutable |
| `execution.maxResidentCells` | Hard resident-cell admission cap (`CELLD_MAX_RESIDENT_CELLS`); unset keeps the runtime default; immutable |
| `execution.idleEvictSeconds` | Idle hibernation age (`CELLD_IDLE_EVICT_S`); unset leaves only pressure and the cap to evict; immutable |
| `lifecycle.shutdownSeconds` | celld total stop budget (`CELLD_SHUTDOWN_TOTAL_MS`); default 20; immutable |
| `lifecycle.terminationGraceSeconds` | Pod termination grace; default 30; at least shutdown + 5; the launcher escalates 5 s before it; immutable |

Fleet names must be DNS labels up to 40 characters. Only `replicas`, `capacity`, `runtimeImage`, `maintenance`, and an authorized Deployment-to-Ordered layout migration are mutable; `execution` and `lifecycle` tuning are fixed at creation because the operator never rolls out a changed pod template (see [ADR 0018](decisions/0018-per-fleet-tuning.md) and the [tuned example](../config/samples/tuned.yaml)).
Invalid cross-field combinations fail admission; name/dependency errors also
produce clear controller conditions. The `/scale` subresource maps `spec.replicas`,
`status.replicas` (non-terminal pods, including terminating ones) and
`status.labelSelector`; writes through it are ordinary `spec.replicas` edits and
pass every lifecycle gate (see [External mode](capacity-policy.md#external-mode)). Storage resize, storage changes and placement changes are unsupported. Runtime
updates follow the explicit [version transition](runtime-versions.md) procedure. Manual scale-out is journaled. Bucket reductions execute under [logical membership completion](bucket-scale-in.md); launcher-managed PersistentFleet reductions execute through the [graceful retirement path](persistent-fleet-lifecycle.md), and fleets without the launcher remain blocked by the fencing gates. See [ADR 0012](decisions/0012-restart-safe-manual-lifecycle.md) and [ADR 0017](decisions/0017-persistent-launcher-and-graceful-retirement.md).

The application Service is `<fleet>:8080`; label authorized client/ingress pods
in that namespace `celld.eric.dev/client-of: <fleet>`. Internal port 8081 is
private to same-fleet peers and the operator namespace's pods labeled
`app.kubernetes.io/name: celld-operator`. Do not route the peer Service through
external ingress. Public ingress, TLS and DNS remain user-managed. Runtime peer
addresses are individual Pod IPs; headless `<fleet>-peers` also enables DNS discovery.

`Ready` means the current workload has at least one replica and all its applied
replicas are runtime-ready. It may be true while the fleet target differs. It is not evidence of durable writes or follower placement.
`InfrastructureReady` means the required objects match, even while Pods are
Pending. `Blocked` identifies invalid configuration, missing dependencies,
isolation verification, conflicting reservations, drift or missing workloads.
`LifecycleBlocked=True` and `ProductionQualified=False` remain explicit throughout.
Use Pod events to distinguish insufficient nodes, AZ constraints, PVC binding and
runtime health 503. Metrics serving is optional through `--metrics-bind-address`;
Prometheus is never a provisioning prerequisite.

Deletion waits behind a finalizer while shutdown is verified. After verified
shutdown, compute cleanup and finalizer completion proceed; PVCs, bucket data,
reservation and recovery records remain retained. Do not remove that finalizer as a routine cleanup procedure.
Initial PersistentFleet provisioning rejects pre-existing ordinal PVCs, including
claims carrying matching labels. It exclusively creates new claims before the
StatefulSet can consume them. A claim-name race or partial allocation preserves
all claims and blocks the workload. A failed creation attempt can require review
even if no workload exists; that is
a deliberate response to uncertain history. The prototype never frees a bucket
reservation or attaches another fleet UID to it. Administrative cleanup must first
establish runtime fencing, preserve recovery evidence/disks and exclude external
writers; a supported automated cleanup procedure is future lifecycle work.

`--local-test` is exclusively for the disposable integration harness: it selects
local MinIO, synthetic credentials and a test storage provisioner. Never enable
it on EKS. It does not accept or infer AWS credentials or a default cluster. With `--local-evidence`, the in-cluster fixture installs the fixed MinIO evidence transport; this flag requires `--local-test`.

## Validation

`make check` runs build, race tests and lint. `make test-envtest` runs the
reconciler and journal against a real kube-apiserver and etcd.
`CELLD_DOCKER_TEST=1 go test -race ./internal/controller -run TestLauncher -v`
checks the Bucket profile's shell start-up wait (the full TTL delay before exec
and termination during that delay) in network-disabled Docker containers; the
PersistentFleet launcher binary has its own suite under `internal/launcher`.
`make generate` updates deepcopy and
CRD artifacts; `make manifests-check` verifies reproducibility without rewriting
them. `make integration` creates a unique kind cluster and dedicated kubeconfig,
installs SHA-checked Calico, local MinIO and local-path persistent volumes, runs the
real pinned runtime and operator with its ServiceAccount RBAC, tests strict missing-zone blocking, then deletes only
that invocation's cluster. It never uses a pre-existing cluster. Docker images and
build caches may remain. Tests need Docker, kind, kubectl, network access and enough
memory for three Kubernetes nodes and at least six runtime pods. No AWS qualification follows
from this local test, and local-path disk recovery is not EBS recovery. The
`integration-bucket`, `integration-persistent`, `integration-ordered-bucket`,
`integration-maintenance`, `integration-faults` and `integration-persistent-rwop` targets run the in-cluster
manager with Metrics Server and the fixed MinIO evidence transport for the
corresponding lifecycle paths; see [fault injection](qualification/faults/README.md).

Recorded results and remaining limits: [step 2 evidence](qualification/infrastructure/README.md).

## Manual lifecycle status

Edit `spec.replicas` to request additive capacity. Both profiles preserve the
workload UID and template. `status.lifecycle` exposes the operation ID, phase,
from/to counts, selected target/session (when present) and sticky loss finding.
The retained reservation journal is authoritative; clearing status does not cancel
an operation. The `Progressing` condition with reason `LifecycleProgress` means a durable transition is in progress.
`ScaleOutBlocked` identifies uncertain PVC allocation or replica update conflicts.
`StorageIdentityConflict` identifies missing/replaced retained disks.

Contraction executes for Bucket fleets and for launcher-managed PersistentFleet
fleets (see [Bucket contraction](bucket-scale-in.md) and
[PersistentFleet lifecycle](persistent-fleet-lifecycle.md)). `BucketCompletionUnqualified`
appears when the manager has no evidence transport (no `--local-evidence` and no
production reader); `FencingUnqualified` appears for PersistentFleet fleets
without the launcher. Production automatic contraction reports
`BucketAutomaticUnqualified` or `PersistentAutomaticUnqualified` until the AWS
release gate closes. No timeout or administrative attestation bypasses these gates.

Legacy PersistentFleet reservations without recorded creation PVC UIDs block
for review. The operator does not infer identity from matching labels or offer an
automatic migration. See [step 3 validation](qualification/lifecycle/README.md).

## Maintenance

See [the pause example](../config/samples/maintenance-paused.yaml) and
[ADR 0014](decisions/0014-coordinated-maintenance.md) for exact pause, resume,
request precedence, finalizer and garbage collection semantics.
`MaintenancePaused` and `Deleting` conditions expose the control state;
`status.lifecycle.requestKind`, `requestID` and `targetImage` project the retained
blocked request. Same-version restart and one directional PersistentFleet runtime upgrade are
implemented with explicit prerequisites; see [maintenance](maintenance-execution.md)
and [runtime versions](runtime-versions.md). Rollback remains unsupported.

Pause is asynchronous: inspect the condition and workload maintenance fence.
Already-issued effects continue recovery; uncertain completion and loss remain
blockers. Deletion retains storage and recovery identities after compute cleanup. Never delete a reservation to reuse a bucket or retained disk.

Historical [maintenance validation](qualification/maintenance/README.md).

## Review corrections

Strict placement now requires same-fleet pods to use distinct hostnames, in
addition to the zone constraints. Insufficient eligible hosts leave pods Pending.
Relaxed placement keeps the AZ allowlist but makes both hostname separation and
zone balance preferences. Node separation still does not establish runtime
follower AZ diversity.

The stronger pod template applies to newly provisioned fleets. Existing templates
without hostname anti-affinity are detected as drift; the operator does not roll
out a disruptive template repair. They need a separately qualified migration.
Do not delete reservations or edit journals to bypass that boundary.

`status.readyReplicas` and `Ready` now describe the owned workload's observed
availability, independent of desired scaling or maintenance permission. A paused
fleet or an unsupported restart/image request can remain Ready while Blocked is
also true. Workload status must cover its current generation; missing or replaced
fleet identities cannot supply availability. `Ready` is not a recovery certificate.

The journal is currently version 8. Every earlier version (1 through 7) is read
conservatively and rewritten as 8 on the next durable write; an older operator
binary rejects a newer journal rather than ignoring authority it does not know.
Downgrading after advancement is not a supported rollback procedure. The version
steps and what each added are recorded in the ADRs; the compatibility rules and
archive format are in [journal archives](journal-archives.md).

`status.lifecycle.retiredBucketSessions` counts retained Bucket generations outside observed membership whose physical process liveness remains unknown. It is not a count of fenced processes.
