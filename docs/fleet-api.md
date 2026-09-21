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
`maintenance`. Storage, layout, placement, execution sizing, lifecycle budgets,
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
See [current operations](current-operation.md) before changing lifecycle code.

The `/scale` subresource maps desired replicas to `spec.replicas` and observed
nonterminal Pods to `status.replicas`. External autoscalers target the CelldFleet,
never the managed Deployment or StatefulSet. They gain no deletion authority.

Install with namespace-scoped RBAC for every managed namespace and attest CNI
NetworkPolicy enforcement. See [installation](../site/src/content/docs/start/install.mdx)
and [security boundaries](../site/src/content/docs/reference/security-boundaries.md).
