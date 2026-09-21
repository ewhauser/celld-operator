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
`maintenance`. Storage, layout, placement, execution sizing and lifecycle budgets
are fixed at creation. `maintenance.paused` stops new and unissued work;
`allowCoordinatedDowntime` permits a whole-fleet restart or upgrade. A new
`restartToken` requests a same-version restart.

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
