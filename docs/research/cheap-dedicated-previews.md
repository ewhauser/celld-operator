# Cheap dedicated previews

Investigation date: 2026-09-23. This report evaluates isolated, single-node
fleets sharing cluster infrastructure. It does not introduce a new operator
profile or qualify production deployment.

## Decision

Keep one runtime per **active** preview as the first implementation direction.
Each preview retains its own URL, code, bindings and storage prefix. Share
nodes, ingress, wildcard TLS and the object-storage service. Make sleeping
previews a first-class lifecycle state.

There are two materially different storage choices:

1. **Retained previews:** external object storage and existing bucket durability.
   Sleep inactive previews to stop runtime bookkeeping traffic. This is the
   conservative first implementation using the released runtime.
2. **Disposable previews:** a shared, cluster-local S3-compatible store with
   explicitly relaxed availability and data retention. Independent prefixes
   still give each runtime separate state. This avoids an external per-request
   bill; the storage service still consumes compute, memory and disk. The local
   experiments use this topology, but do not qualify a deployed storage service.
   Loss of the store may reset every affected preview.

Do not introduce one object-store process, PVC or load balancer per preview.
Do not assume smaller Kubernetes resource requests alone make thousands of
always-running fleets cheap.

## Measured outcome

The local test reached 100 active isolated runtimes: about 1.56GiB combined
working memory idle and 2.05GiB after one Durable Object per fleet, plus shared
storage/proxy overhead. All 100 write/read pairs passed. A 128Mi container
limit passed the separate 32-object fixture; 64Mi stalled. These are small-app
Docker measurements, not Kubernetes capacity guarantees.

The expensive floor is remote coordination: the corrected idle run projected
to about $9,620/month in S3 Standard request charges at 1,000 always-on fleets.
Slower leases/polling and disabled balancing still projected to about
$6,268/month. Both are extrapolations using measured local request counts and
current regional prices; the evidence report contains the calculation and its
limits. Configuration changes alone do not eliminate this floor.

## Implementation status after investigation

The operator now exposes `CelldPreviewPool` plus `CelldPreview.spec.poolRef`.
It implements shared bucket prefixes with permanent pool/prefix reservations,
custom object-store endpoints with Secret references, small execution defaults,
and separate scratch requests/limits. See [application previews](../previews.md)
for the implemented API. Idle suspension/wake-on-request remains unimplemented;
always-running S3-backed previews still incur the request floor measured below.
The constraints in the next section describe the baseline investigated before
this refactor.

## Baseline implementation constraints

The operator defaults to 250m CPU request, 512Mi memory request, 1Gi memory
limit and 10Gi ephemeral-storage request/limit for a Bucket pod. One thousand
single-pod fleets therefore request 250 CPU cores, 500Gi memory and 10,000Gi
scratch capacity. These are scheduler reservations, not measured consumption.
`execution` already permits smaller CPU/memory requests, residency caps and
idle eviction. `spec.env` supports additional runtime request/isolate bounds.

Scratch capacity is less flexible: `storage.sizeGiB` has a 1Gi minimum and
becomes both the emptyDir limit and the ephemeral-storage request. Split the
request from the limit and permit smaller quantities for the preview profile.
The runtime log filter (`RUST_LOG`, default `info`) is also absent from the
current environment customization API; account for log ingestion and rotation
at scale.

Use a bounded disk-backed emptyDir, rather than charging SQLite/cache storage
against a tiny memory limit with tmpfs. The runtime preserved-cache default is
2GiB (`CELLD_LOCAL_CACHE_MAX_BYTES`); reduce that too when qualifying a smaller
scratch limit. That cap only covers preserved cache, so leave room for live
SQLite files, journals and launch metadata. Disk exhaustion needs its own test.

Runtime storage accepts `s3://bucket/prefix`; the operator reserves a whole
bucket and renders `s3://bucket`. Supporting a shared bucket needs a real,
immutable prefix field, prefix-aware scope reservations and cleanup. Reject
overlapping prefixes, and preserve identity across suspension and resumption.
The benchmark uses trusted local credentials shared by every process: its
prefix-isolation checks are not a security or IAM qualification. Use scoped
credentials where previews represent separate trust domains.

Custom S3 endpoints need an explicit storage configuration surface. Today the
operator only injects `S3_ENDPOINT` in its local-test path; `spec.env` accepts
CELLD variables rather than arbitrary AWS/S3 endpoint settings.

The operator requires at least one replica. Scale-to-zero needs lifecycle work,
not a direct write to the generated Deployment/StatefulSet. Keep the fleet's
storage reservation and Preview URL while suspended; do not delete/recreate
the fleet to implement sleep. Its terminal deletion proof and missing-child
handling deliberately prevent that form of resurrection.

The real launcher unconditionally waits ten seconds before starting celld.
Any shorter cold-start target requires an independently justified change to
that supervision contract; image startup alone is not the relevant latency.

## Initial sizing candidate

Start Kubernetes qualification with a single Bucket replica, a 25m CPU
request, 64Mi memory request and 256Mi memory limit. These are candidate
reservations, not measured capacity guarantees. Keep burst CPU available and
apply explicit admission bounds: one stateless isolate, eight in-flight
requests, four requests per cell, eight resident cells, 30-second idle eviction
and a 1MiB request body cap. Tune per application; streaming uploads and larger
bundles need different limits. The tiny fixture passed a 128Mi limit but stalled
at 64Mi. A larger default gives room for real applications.

At 1,000 *active* previews those candidate requests are 25 CPU cores and
62.5Gi memory, before shared services and Kubernetes overhead. Only sleeping
changes the number of resident runtimes. Reduced reservations alone do not
eliminate resource contention or storage traffic.

## Developer contract

Platform configuration should own sizing, storage endpoints, wildcard routing
and idle policy. A developer supplies application/revision and a preview
profile, with a stable `status.url` on the Preview. They should not copy an
entire fleet specification or supply a new bucket for every branch.

Two explicit guarantees are useful:

- Retained: acknowledged state survives supported sleep/restart; requests wait
  during a cold start and fail after a bounded deadline.
- Disposable: state can reset following infrastructure loss or expiry. Report
  reset generation/reason in status instead of presenting an unexplained empty
  database as successful retained recovery. Keep the immutable application
  bundle outside the disposable store, or retain a reliable way to redeploy it;
  resetting the object store otherwise loses code/configuration as well as data.
  Reseed the application before marking a reset preview ready.

For both, cap request size, concurrency, resident objects, local cache and total
storage; reject overload visibly. Accept slower cold objects and modest
throughput. Do not relax cross-preview isolation, fencing or unique routing.

Sleeping changes background execution: Durable Object alarms, workflows, cron
and queues cannot run in a stopped runtime. Initially require HTTP-driven
previews or explicitly document delayed background work. Keep WebSockets and
active requests from triggering sleep. Later add a shared wake scheduler if
timely background execution is required.

## Implementation sequence

1. Add platform-owned preview profiles and existing execution limits. Preserve
   one unique hostname per Preview with shared ingress/TLS. Decouple scratch
   request from limit. Keep normal fleet defaults unchanged.
2. Add bucket/prefix scope identity and optional object-store endpoint. Test
   overlapping-prefix rejection, independent code updates, same-name DO/KV/D1
   isolation, deletion races and expiry cleanup. The small test app covers DO
   storage only, not the complete bindings matrix.
3. Add suspend/resume through the operator lifecycle boundary. A shared ingress
   activator queues a bounded number of requests, requests wake through the
   Preview/controller API, and forwards only after readiness. Preserve the URL
   and storage identity across sleep. Test concurrent wake, delete during wake,
   controller restart and unsuccessful shutdown proof.
4. Qualify 100 and then 1,000 Preview objects in a dedicated Kubernetes cluster,
   including mostly asleep populations, an all-active case and a wake storm.
   Measure API server/controller cost, routing convergence, pod/IP capacity,
   p95/p99 request and wake latency, resource pressure and storage traffic.

[KEDA's HTTP interceptor](https://keda.sh/http-add-on/0.16/concepts/architecture/)
is a possible component for request buffering; assess its compatibility with
the operator's scale boundary before selecting it. Do not let it bypass fleet
shutdown checks by scaling the generated workload directly.

Kubernetes' published large-cluster guidance includes at most 110 pods per
node. Even if memory fits, 1,000 active one-pod fleets need at least ten nodes
under that envelope, before system pods and CNI/IP restrictions. This is a
planning constraint, not a universal hard-coded limit.
[Kubernetes guidance](https://kubernetes.io/docs/setup/best-practices/cluster-large/).

## If every preview must stay active

If the target is 1,000 simultaneously active previews, use a shared local
object store or first remove the per-fleet remote coordination cost. Sleeping
only helps when the active fraction is small. Preserve conditional-write and
fencing semantics; simply disabling leases is not a valid optimization.

There is a later option to run several independent celld processes under a
shared preview-host supervisor, avoiding one Kubernetes pod and controller
resource graph per preview. This is distinct from co-hosting applications in
one celld deployment graph: it retains separate processes and storage scopes.
It also introduces process scheduling, per-preview quotas, port allocation,
recovery and isolation responsibilities. Do not build that new hosting layer
before measuring the simpler one-pod model in Kubernetes.

## Evidence

See [the local experiment report](../qualification/preview-density/README.md)
for measurements, raw observations, cost calculations and reproduction commands.
No operator/runtime behavior was changed by this investigation.
