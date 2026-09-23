# Fleet API

`celld.eric.dev/v1alpha1` defines a namespaced `CelldFleet` and a cluster-scoped
`CelldStorageReservation`. The generated [CRD](../config/crd/celld.eric.dev_celldfleets.yaml)
is the field and admission reference.

A fleet supplies an explicit compatible runtime digest, an existing runtime
ServiceAccount, a dedicated bucket, region and zone allowlist. The operator
requires a digest-pinned launcher image for both profiles. The operator creates
workloads, private Services, NetworkPolicies, a PodDisruptionBudget and a launcher
credential. PersistentFleet also uses CSI claims.

Bucket has immutable `Deployment` or `Ordered` layout; Ordered provides exact
highest-ordinal contraction. PersistentFleet uses a StatefulSet. Neither layout
is converted in place. Infrastructure outside Kubernetes, including bucket,
IAM roles, worker nodes and EBS CSI, remains administrator-owned.

The mutable request fields are `replicas`, `capacity`, `runtimeImage` and
`maintenance` and `routing`. Storage, layout, placement, execution sizing, lifecycle budgets,
`env`, and `telemetry` are fixed at creation. `maintenance.paused` stops new and unissued work;
`allowCoordinatedDowntime` permits a whole-fleet restart or upgrade. A new
`restartToken` requests a same-version restart.

`spec.env` adds up to 32 `CELLD_` variables with either a literal `value` or a
same-namespace `secretKeyRef` (`name` and `key`). Operator-owned identity,
network, durability, storage and launcher settings cannot be overridden. The
OpenTelemetry namespace is reserved for its dedicated API. Environment entries
cannot be edited after fleet creation because the operator does not roll out
ordinary pod-template changes. Secret values never enter the fleet API, status,
or operator logs; Kubernetes resolves references when a Pod starts. Rotating a
Secret does not update running processes: schedule coordinated maintenance to
restart the fleet after a rotation.

`spec.telemetry` enables OTLP export to `collectorURL`; omission leaves it
disabled. The operator emits `CELLD_OTEL=<collector URL>`, optional sampler and
flush settings, and optional `OTEL_EXPORTER_OTLP_HEADERS` from a same-namespace
Secret key. It never emits the removed `CELLD_OTEL_SINK`. `egress` is required
and adds one TCP rule for the collector URL's port, scoped to one IP address
(`cidr` /32 or /128) or labeled collector Pods (`podLabels`, with optional
`namespace`). The administrator must ensure the URL resolves to the selected
destination. The existing broad HTTPS rule for S3 and STS still permits other
port 443 destinations; NetworkPolicy alone does not make HTTPS collector traffic
exclusive to the telemetry egress selector. A collector using a different port
receives only its scoped rule. Telemetry is immutable after creation because
ordinary pod-template changes are not rolled out. Rotate the headers Secret
with coordinated maintenance so new Pods read it.

A bucket reservation permanently binds the bucket to the fleet UID. Recreating
a fleet with the same name does not transfer ownership. Its current-operation
annotation is controller authority; status is a reconstructible projection.
[Preview pools](previews.md) instead reserve the bucket to a pool UID, then bind
each child fleet to a disjoint single-segment `storage.prefix` and `poolRef`.
A pool-generated fleet may set a Secret-backed custom `storage.endpoint` and
separate `storage.scratch.request`/`limit` for its disk-backed emptyDir. These
fields are immutable and do not change ordinary dedicated fleet defaults.
See [current operations](current-operation.md) before changing lifecycle code.

The `/scale` subresource maps desired replicas to `spec.replicas` and observed
nonterminal Pods to `status.replicas`. External autoscalers target the CelldFleet,
never the managed Deployment or StatefulSet. They gain no deletion authority.

Install with namespace-scoped RBAC for every managed namespace and attest CNI
NetworkPolicy enforcement. See [installation](../site/src/content/docs/start/install.mdx)
and [security boundaries](../site/src/content/docs/reference/security-boundaries.md).

`spec.routing` optionally creates one Gateway API v1 HTTPRoute or a standard
Ingress, named `FLEET-routing`. Both forward `/` for explicit `hostnames` to
`FLEET:8080`. A required `source` namespace and Pod label selector admit only the
selected data-plane Pods through a separate NetworkPolicy. Routing never exposes
8081 or 8083. Gateway mode references an existing Gateway; Ingress mode specifies
an IngressClass and optionally a same-namespace TLS Secret and annotations.

Routing can be added, edited, switched or removed without changing the workload
or storage reservation. The operator updates only resources controlled by the
current fleet UID, deletes with UID/resourceVersion preconditions and waits for
obsolete routes to disappear before creating a replacement. Before removing an
existing route, it checks the selected API, Service, isolation setting and
resource ownership; a failed preflight preserves the existing route and policy.
Removed managed annotations are pruned; unrelated controller annotations are preserved. Removing
`routing` deletes the owned route, then its ingress policy. Routing also closes
when fleet deletion starts; it does not delay strict runtime recovery.

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
publishes code, calls `/reload`, changes replicas or authorizes runtime removal.

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
