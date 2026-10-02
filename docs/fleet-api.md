# Fleet API

`celld.eric.dev/v1alpha1` defines a namespaced `CelldFleet` and a cluster-scoped
`CelldStorageReservation`. The generated [CRD](../config/crd/celld.eric.dev_celldfleets.yaml)
is the field and admission reference.

A fleet supplies an explicit compatible runtime digest, an existing runtime
ServiceAccount, a dedicated bucket, region and zone allowlist. The operator
creates workloads, private Services, NetworkPolicies and a PodDisruptionBudget.
Pods run celld directly: Bucket with `CELLD_DURABILITY=bucket`, PersistentFleet
with `CELLD_DURABILITY=fleet` on retained CSI claims.

Bucket has immutable `Deployment` or `Ordered` layout; both contract one member
at a time, and Ordered removes the highest ordinal. PersistentFleet uses a
StatefulSet. Neither layout
is converted in place. Infrastructure outside Kubernetes, including bucket,
IAM roles, worker nodes and EBS CSI, remains administrator-owned.

The mutable request fields are `replicas`, `capacity`, `runtimeImage`,
`maintenance`, `export`, `routing` and `mesh`. Storage, layout, placement, execution sizing, lifecycle
budgets, `env` and `telemetry` are fixed at creation. `maintenance.paused` stops new
workload changes. A new `restartToken` requests a same-version restart. Restarts
and upgrades are one-member rolling updates for both profiles;
`allowCoordinatedDowntime` is accepted and ignored.

PersistentFleet has no member-replacement API. The operator replaces a member
that cannot come back on its own: when its claim is `Lost`, or after the
replacement delay when it is the one member down and the rest of the fleet is
ready. The StatefulSet creates a fresh claim. See
[self-healing](current-operation.md#self-healing).

`spec.env` adds up to 32 `CELLD_` variables with either a literal `value` or a
same-namespace `secretKeyRef` (`name` and `key`). Operator-owned identity,
network, durability and storage settings and reserved prefixes such as
`CELLD_REEXEC_` and `CELLD_STRICT_` cannot be overridden. The
OpenTelemetry namespace is reserved for its dedicated API. Environment entries
cannot be edited after fleet creation because the operator does not roll out
ordinary pod-template changes. Secret values never enter the fleet API, status,
or operator logs; Kubernetes resolves references when a Pod starts. Rotating a
Secret does not update running processes: change `maintenance.restartToken` to
restart the fleet after a rotation.

`spec.telemetry` enables OTLP export to `collectorURL`; omission leaves it
disabled. The operator emits `CELLD_OTEL=<collector URL>`, optional sampler and
flush settings, and optional `OTEL_EXPORTER_OTLP_HEADERS` from a same-namespace
Secret key. It also emits `OTEL_RESOURCE_ATTRIBUTES` with
`k8s.namespace.name`, `k8s.pod.name` (from `POD_NAME`, set by the downward API)
and `celld.fleet.name`, so celld's OTLP signals join the operator's
`celld_fleet_*` metrics on namespace and fleet; celld reads it from
`0.6.0-ewhauser.2`. It never emits the removed `CELLD_OTEL_SINK`. `egress` is required
and adds one TCP rule for the collector URL's port, scoped to one IP address
(`cidr` /32 or /128) or labeled collector Pods (`podLabels`, with optional
`namespace`). The administrator must ensure the URL resolves to the selected
destination. The existing broad HTTPS rule for S3 and STS still permits other
port 443 destinations; NetworkPolicy alone does not make HTTPS collector traffic
exclusive to the telemetry egress selector. A collector using a different port
receives only its scoped rule. Telemetry is immutable after creation because
ordinary pod-template changes are not rolled out. Rotate the headers Secret
with a restart so new Pods read it.

`spec.export` turns on celld's
[change export](https://github.com/ewhauser/celld/blob/main/docs/export.md),
which streams every committed change to the cells' SQLite databases out of
celld; omission leaves it disabled. The operator emits `CELLD_EXPORT=1` and
the `CELLD_EXPORT_*` settings below, and reserves the `CELLD_EXPORT` prefix in
`spec.env`. Omitted fields keep celld's defaults.

| field | celld setting |
| --- | --- |
| `sink` | `CELLD_EXPORT_SINK`: `Bucket` (default) or `Kafka` |
| `classes` | `CELLD_EXPORT_CLASSES`, the classes to export |
| `excludeTables` | `CELLD_EXPORT_TABLES`, `Class.table` entries never exported |
| `maxTransactionBytes`, `maxRecordBytes`, `queueBytes` | `CELLD_EXPORT_MAX_TX_BYTES`, `CELLD_EXPORT_MAX_RECORD_BYTES`, `CELLD_EXPORT_QUEUE_BYTES` |
| `bucket.name` | `CELLD_EXPORT_BUCKET`, an export bucket other than the fleet bucket |
| `bucket.flushMilliseconds`, `bucket.flushBytes` | `CELLD_EXPORT_FLUSH_MS`, `CELLD_EXPORT_FLUSH_BYTES` |
| `bucket.retentionDays` | `CELLD_EXPORT_RETENTION=<n>d` |
| `kafka.brokers`, `kafka.topic`, `kafka.retryMilliseconds` | `CELLD_EXPORT_KAFKA_BROKERS`, `CELLD_EXPORT_TOPIC`, `CELLD_EXPORT_RETRY_MS` |
| `kafka.propertiesSecretKeyRef` | `CELLD_EXPORT_KAFKA_PROPERTIES=/etc/celld/export/<key>`; the whole Secret is mounted read-only there |

The Bucket sink writes Parquet objects under `export/changes/` in the fleet
bucket, or in `bucket.name` on the same endpoint and credentials, so the
fleet's ServiceAccount must be able to write there. It needs no new egress.
The Kafka sink needs `kafka.egress`, which adds one TCP rule for the brokers'
ports to labeled broker Pods (`podLabels`, with optional `namespace`) or to a
`cidr`, which may cover a whole network such as a managed cluster's subnets.
Create the topic before the fleet; the sink never creates it. librdkafka
settings for TLS, SASL, compression and batching go in the properties key, one
`name=value` per line, so SASL credentials stay in the Secret. Every key of the
Secret is mounted, so a private CA or client certificate rides in the same
Secret and the properties name its file, for example
`ssl.ca.location=/etc/celld/export/ca.crt`. celld refuses properties that
weaken delivery: `acks` below `all`, `message.timeout.ms`,
`delivery.timeout.ms`, and turning on `delivery.report.only.error` or
`allow.auto.create.topics`. Kafka needs a celld
built with the `export-kafka` feature, which the fork's release images leave
out, so it also needs a custom `runtimeImage`. The operator does not support
celld's blob-stream sink, and the reserved `CELLD_EXPORT` prefix keeps
`spec.env` from selecting it.

The export queue (`queueBytes`, 256 MiB by default) is held in the celld
process, so size `execution.memoryLimit` for it. Export starts with the cells a
node activates once it is on; backfill earlier cells and schedule the
reconciler with the `celld export` commands. celld reports export gauges
(`celld.export.*`) through `spec.telemetry`. Change export needs a celld
build that has it: see [runtime requirements](runtime-versions.md#change-export).
Adding, changing or removing export rolls members one at a time. The egress
policy follows the new spec at the start of the rollout, so a member that still
runs the old export can lose its sink until it is replaced; its queue fills and
drops records as gaps rather than blocking writes. Cells activated on members
that did not export yet are not exported: run the backfill once the rollout
completes. A fleet created with export by v0.0.8 or v0.0.9 keeps its
reservation only while that export is unchanged; recreate it to change export.

A bucket reservation permanently binds the bucket to the fleet UID. Recreating
a fleet with the same name does not transfer ownership. Its
`celld.eric.dev/current-operation` annotation holds only capacity-policy
history and is removed when there is none; fleet status is a reconstructible
projection.
[Preview configuration](previews.md) lives on an existing fleet under `spec.previews`.
It reserves a separate preview bucket to that parent fleet UID, then binds each
child fleet to a disjoint `storage.prefix` and `previewFleetRef`.
A generated preview fleet may set a Secret-backed custom `storage.endpoint` and
separate `storage.scratch.request`/`limit` for its disk-backed emptyDir. These
fields are immutable and do not change ordinary dedicated fleet defaults.
`spec.previews` may be added once; it is immutable thereafter and excluded from
the parent runtime reservation hash. Initialization requests and executor status
use the existing `CelldStorageReservation`; its `current-operation` annotation
remains operator-owned, while its status subresource holds the retained seed result.
See [PersistentFleet lifecycle](current-operation.md) before changing lifecycle code.

The `/scale` subresource maps desired replicas to `spec.replicas` and observed
nonterminal Pods to `status.replicas`. External autoscalers target the CelldFleet,
never the managed Deployment or StatefulSet. They gain no deletion authority.

Install with namespace-scoped RBAC for every managed namespace and attest CNI
NetworkPolicy enforcement. See [installation](../site/src/content/docs/start/install.mdx)
and [security boundaries](../site/src/content/docs/reference/security-boundaries.md).

`spec.mesh.istio` places members in an Istio sidecar mesh that may run STRICT
mTLS and AuthorizationPolicies. The Pod template gains the
`sidecar.istio.io/inject: "true"` and `celld.eric.dev/fleet: FLEET` labels and
asks for a native sidecar (`sidecar.istio.io/nativeSidecar: "true"`), which
starts before celld and stops after it, so peer RPC works through startup
recovery and the SIGTERM handoff. The EKS Pod Identity agent address bypasses
the proxy. The fleet NetworkPolicy adds egress to istiod Pods (`app: istiod`) in
`controlPlaneNamespace` (default `istio-system`) on TCP 15012. The operator
owns an ALLOW AuthorizationPolicy named `FLEET-mesh` that admits port 8081 only
from the fleet's ServiceAccount and the operator's (`--operator-service-account`),
matched in any trust domain. With `applicationAccess: AllowAll` (the default) it
also allows port 8080 from any caller; with `Policies`, port 8080 callers need an
AuthorizationPolicy you write. The policy is created before the workload; a
missing `security.istio.io/v1` API or a same-named policy the fleet does not own
reports `InfrastructureBlocked`. `controlPlaneNamespace` and `applicationAccess`
change only policies and roll nothing. Adding or removing `mesh` rolls members one at
a time. Until every member Pod matches, the operator keeps a PeerAuthentication
`FLEET-mesh` that makes port 8081 `PERMISSIVE` and drops the identity requirement
from the peer-port rule, since an unmeshed member's plaintext peer RPC has no
identity; the fleet NetworkPolicy still limits 8081 to members and the operator.
It checks actual proxy containers, including native sidecars and terminating
Pods, before restoring the strict rule and deleting the PeerAuthentication.
Missing injection keeps the fleet in `Provisioning`. Departure retains istiod
egress until the final proxy exits, then deletes both mesh objects and closes
that egress. Replacement Pods disable injection explicitly; the retained fleet
annotation `celld.eric.dev/istio-control-plane` keeps this opt-out after operator
restarts, even in a namespace configured for injection. Fleets that never opted
in keep their previous template. The operator reads each member's `/state`
on 8081 directly, so under STRICT mTLS the operator Pods must be in the mesh too.
See [Istio networking](../site/src/content/docs/configure/networking.md#istio-strict-mtls-and-authorization).

`spec.routing` optionally creates one Gateway API v1 HTTPRoute or a standard
Ingress, named `FLEET-routing`. Both forward `/` for explicit `hostnames` to
`FLEET:8080`. A required `source` namespace and Pod label selector admit only the
selected data-plane Pods through a separate NetworkPolicy. Routing never exposes
8081. Gateway mode references an existing Gateway; Ingress mode specifies
an IngressClass and optionally a same-namespace TLS Secret and annotations.

Routing can be added, edited, switched or removed without changing the workload
or storage reservation. The operator updates only resources controlled by the
current fleet UID, deletes with UID/resourceVersion preconditions and waits for
obsolete routes to disappear before creating a replacement. Before removing an
existing route, it checks the selected API, Service, isolation setting and
resource ownership; a failed preflight preserves the existing route and policy.
Removed managed annotations are pruned; unrelated controller annotations are preserved. Removing
`routing` deletes the owned route, then its ingress policy. Routing also closes
when fleet deletion starts; it does not delay runtime shutdown.

Reserved or oversized Ingress annotations are rejected at admission. Invalid
routing stored by older schemas or admission bypasses reports `RoutingReady=False`
without blocking runtime operations.

`RoutingReady` is separate from fleet readiness. HTTPRoute acceptance requires
current-generation `Accepted=True` and `ResolvedRefs=True` for the configured
parent. Ingress has no portable acceptance condition: an assigned address is
reported, or `AwaitingAddress` remains Unknown. Neither observation proves DNS,
TLS, Gateway listener programming or application connectivity. Missing optional
Gateway CRDs report `GatewayAPIUnavailable` without blocking runtime operations.
The operator polls routing status on normal reconciliation; it does not watch or
provision Gateway, IngressClass, certificate or DNS resources. See the
[networking guide](../site/src/content/docs/configure/networking.md) and the
[routing samples](../config/samples/routing-gateway.yaml).

## Application deployment observations

`status.application` and the `ApplicationConverged` condition describe application
code loaded by the runtime. They are independent of the container image digest,
Kubernetes object generation and fleet lifecycle readiness. Observation never
publishes code, calls `/reload`, changes replicas or authorizes disk deletion.

The operator reads the existing private `GET /state` endpoint at most once every
15 seconds per fleet, with eight concurrent requests, a 10-second collection
deadline and a 1 MiB limit per response. This requires no new runtime patch or
image update. Missing deployment fields, oversized responses and unavailable
nodes yield Unknown while normal lifecycle operations continue.

Status contains a collection timestamp, expected and freshly observed node
counts, unavailable coverage, loaded versions grouped by version and artifact
prefix, pending/swapping cell totals and at most 100 Pod diagnostics. Responses
must be at most 90 seconds old. Pod UID, container incarnation, IP, readiness and
workload ownership are checked against opening and closing membership inventories.
Counts omit unavailable samples and are not complete fleet totals when coverage
is incomplete. `observedVersion` is present only with complete fresh coverage and
agreement on the loaded version and prefix; cells may still be transitioning.

| ApplicationConverged | Meaning |
| --- | --- |
| True / Converged | Every current Pod is ready and freshly observed, all report the same loaded version and prefix, and no resident cells are observed pending or swapping. |
| False / MixedVersions | Coverage is complete, but loaded versions or artifact prefixes differ. |
| False / Converging | Nodes agree on the loaded version, but resident cells are still transitioning. |
| Unknown | Observations are stale, unsupported, malformed, incomplete or interrupted by membership changes. |

**This reports observed agreement, not adoption of the latest published release.**
The existing API exposes neither the deployment pointer's freshness nor failed
adoption attempts. All nodes may agree on an old version even when a newer
release cannot be loaded or the pointer is unavailable. A pipeline waiting for a
particular release must compare `status.application.observedVersion.version` and
`prefix` with its expected artifacts, require `ApplicationConverged=True` and
check a fresh `observedAt` and condition `observedGeneration`.

Numeric application generations are process-local and never compared across
nodes. A resident cell is pending when its generation differs from its own node's
current generation; rollback is supported. Celld samples its actor census and
current generation separately, so this is an observation rather than an atomic
rollout-completion guarantee. It does not prove application health or that every
long-lived request from an earlier generation has ended. The condition does not
gate `Ready`, scaling, maintenance or deletion. If the operator stops, persisted
observations age; clients must check timestamps.
