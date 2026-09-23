# Application previews

Developers create a `CelldPreview` referencing an existing `CelldFleet` with
`spec.previews` configured by the platform team. This configuration shares nodes,
routing and one preview object store; each preview gets a small independent runtime, an isolated storage
prefix and its own stable URL. There is no per-preview bucket, PVC, object-store
process or external load balancer. The operator still creates an internal
`CelldFleet` so runtime startup, reservations and shutdown use the existing
lifecycle controls.

## Developer workflow

Once the platform team has enabled previews on a fleet, the developer manifest is:

```yaml
apiVersion: celld.eric.dev/v1alpha1
kind: CelldPreview
metadata:
  name: pr-42
  namespace: previews
spec:
  fleetRef:
    name: development
  source: feature/login
  revision: replace-with-commit-sha
  ttlSeconds: 86400
```

Apply [the preview sample](../config/samples/preview.yaml), then obtain the
upload destination. The operator does not build or upload application code.
Run `celld deploy` from the checked-out branch with your CI deployment identity:

```sh
kubectl --context YOUR_CONTEXT apply -f preview.yaml
kubectl --context YOUR_CONTEXT -n previews wait celldpreview/pr-42 \
  --for=jsonpath='{.status.storageURL}' --timeout=1m
PREVIEW_STORAGE=$(kubectl --context YOUR_CONTEXT -n previews get celldpreview pr-42 \
  -o jsonpath='{.status.storageURL}')
celld deploy ./your-app --bucket "$PREVIEW_STORAGE" --region us-east-1
kubectl --context YOUR_CONTEXT -n previews wait celldpreview/pr-42 \
  --for=condition=Ready --timeout=5m
kubectl --context YOUR_CONTEXT -n previews get celldpreview pr-42 \
  -o jsonpath='{.status.url}{"\n"}'
```

For a custom store, CI also needs `S3_ENDPOINT`, `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, and `AWS_ALLOW_HTTP=true` when using HTTP. The CLI must
reach that endpoint; an in-cluster Service address requires an in-cluster runner
or a suitable tunnel. Use the store configured under the parent's `spec.previews.storage`. Supply credentials through
CI's secret mechanism, not the preview YAML.

Example status:

```yaml
status:
  parentFleetName: development
  fleetName: p-d43be4c2a6434fe6bcadad249dc55493
  storageURL: s3://shared-previews/p-d43be4c2a6434fe6bcadad249dc55493
  url: https://p-d43be4c2a6434fe6bcadad249dc55493.previews.example.com
  phase: Ready
```

The full Kubernetes UID determines the prefix, child fleet and hostname. Updating
code in the same prefix preserves the preview URL and Durable Object state.
Recreating the preview, even with the same name, produces a new prefix and URL.
`spec.revision` is informational CI metadata, not an observed deployment receipt.
`Ready=True` confirms current fleet and route readiness; verify the application,
DNS and TLS with an HTTP request to `status.url` after deploying.

## Clone selected Durable Objects

An optional immutable `spec.seed` selects multiple objects from an administrator-approved
source. The operator reserves the destination and waits for a trusted external
executor to initialize all objects before starting the runtime or enabling routing.
See [preview state seeding](preview-seeding.md) for YAML, executor requirements and
consistency guarantees. The snapshot/import executor is not bundled; seeded
previews remain `Initializing` until one completes the request.

## Platform setup

Install all three CRDs in `config/crd` and upgrade the operator and its RBAC.
Helm includes CRDs for fresh installations; apply CRD updates explicitly when
upgrading. Apply `config/rbac/fleet-namespace.yaml` in each preview namespace, or
configure Helm's `fleetNamespaces`, so the controller can create/delete child
fleets. Give developers access to previews; reserve parent and child fleet configuration
and reservation management for platform administrators.

Adapt [the fleet sample](../config/samples/fleet-previews.yaml). Create the namespace,
ServiceAccount and bucket first. The parent fleet lives in the same namespace as its
previews. Its `spec.previews.storage` requires a fresh bucket separate from the
parent runtime bucket. Choose:

- **Disposable local store:** set `storage.endpoint` to a shared S3-compatible
  service and reference a same-namespace Secret with `accessKeyId` and
  `secretAccessKey` keys. Configure its port and destination labels/CIDR through
  `endpoint.egress`. This avoids external per-request charges; the shared store
  still consumes cluster resources and disk.
- **Retained S3 store:** omit `storage.endpoint` and authorize the runtime
  ServiceAccount for the shared bucket. The runtime continues making coordination
  requests while idle; smaller resource requests do not eliminate that bill.

The bucket is created outside the operator. The parent is an ordinary running fleet; enabling previews does not create
an additional parent runtime or provision an object store. The bucket reservation
is acquired atomically when the first preview uses it. Empty previews copy neither production data nor credentials; optional seeding
copies only the explicitly selected persisted object state. Prefixes isolate runtime state but shared bucket
credentials are **not** an IAM boundary between mutually untrusted tenants. Use
separate parent fleets, preview buckets and identities for separate trust boundaries.

Configure `*.previews.example.com` DNS to the existing edge. For Ingress, provide
a wildcard certificate in the preview namespace and set
`routing.ingress.tlsSecretName`. For Gateway, configure an HTTPS listener for that
wildcard domain, its `allowedRoutes`, and `routing.gateway.sectionName`.
`routing.source` allows ingress from the edge controller's namespace and pod
labels. Reserve this domain for operator previews. The operator does not install
an edge controller, issue certificates, modify DNS, or supply authentication.

A fleet can enable `spec.previews` once; the configuration cannot then change
or be removed. This addition does not change the parent runtime reservation
hash or restart its workload. Use another parent and preview bucket to change
preview infrastructure. A preview's fleet reference and TTL are immutable;
source and revision metadata can be updated. Do not edit generated fleets.

### Optional disposable store sample

[The store sample](../config/samples/preview-store.yaml) supplies one shared MinIO
Deployment, internal Service, ingress NetworkPolicy and runtime ServiceAccount.
Create the `previews` namespace and Secret `preview-store` first using your secret
manager. Apply the sample, then use your object-storage tooling to create the
`shared-previews` bucket at that service before creating previews. For example,
with an authenticated AWS CLI and a reachable endpoint:

```sh
aws --endpoint-url "$S3_ENDPOINT" s3api create-bucket --bucket shared-previews
```

The sample uses disk-backed emptyDir, not a PVC: replacing the store Pod loses
**all** its previews' data. It is intended for disposable, trusted-team previews.
Its 10Gi disk bound and resource limits are starting points, not capacity promises
for thousands of previews. The store NetworkPolicy allows fleet-labelled Pods
and Pods labelled `celld.eric.dev/preview-uploader: 'true'` in the same namespace.
Configure runner egress separately if its namespace has a deny policy. The sample
shares credentials with the store; use a restricted service identity for a
retained or externally managed store.

## Resource defaults and cost boundary

Each running preview requests 25m CPU, 64Mi memory and 64Mi ephemeral storage,
with a 256Mi memory limit and 512Mi disk-backed scratch limit. There is one replica,
relaxed placement within the configured preview zone, eight resident cells, and 30-second
cell eviction. The parent's `spec.previews` can set `execution` and `scratch`.

The runtime also limits stateless isolates to one, global in-flight requests to
eight, cell requests to four, request bodies to 1MiB, and preserved cache to
128MiB or one quarter of scratch, whichever is smaller. Live SQLite files,
journals and logs still need space. These defaults target small development
workloads; monitor throttling, memory pressure and disk exhaustion.

The implementation does **not** suspend idle runtimes or wake on HTTP demand.
Cell eviction releases cells, not the Pod or its coordination traffic. TTL removes
compute through normal shutdown. With 1,000 active previews, default requests
sum to 25 CPU cores, 62.5Gi memory and 62.5Gi scratch, before shared storage,
Kubernetes and edge overhead. Actual use and node cost depend on workload and
packing; this is not a fixed per-preview price.

## Expiry, retention and recovery

TTL defaults to 24 hours from creation, with a range of 60 seconds to seven days.
On PR closure, CI may request immediate cleanup:

```sh
kubectl --context YOUR_CONTEXT -n previews delete celldpreview pr-42 --wait=false
```

Expiry retains the preview object as `Expired` after its child is gone. Explicit
deletion waits for the child fleet's safety finalizer. Both paths remove routing
and request normal fleet shutdown; neither force-removes workloads or bypasses
acknowledged-write checks. An unavailable operator or blocked shutdown can delay
cleanup. Inspect `status.fleetName` and the preview condition message.

Bucket data, the preview bucket reservation and each prefix reservation are retained.
Single-segment prefixes cannot overlap, and permanent reservations cannot transfer
to new Kubernetes UIDs. A normal dedicated fleet cannot claim a reserved preview bucket.
Bucket names are reserved cluster-wide even across different custom endpoints,
preventing aliases from weakening exclusivity. Deleting the parent fleet does not cascade to previews. It blocks new provisioning,
but existing previews can still expire or be deleted through their own lifecycle.
Recreating the parent name cannot adopt its former preview bucket or previews.

Dispose of retained prefix data separately under your retention policy after
safe shutdown. No automatic object-store garbage collection is implemented.
Do not clear reservations to reuse identities. Lost children or ambiguous creation
outcomes report `FleetMissing` even after status loss; inspect the cause and create
a new preview rather than clearing creation-intent annotations or finalizers.

## Verification boundaries

Unit and API-server tests cover disjoint prefixes, reservation races and conflicts,
endpoint/Secret rendering, resource limits, unique routes, immutable configuration,
readiness, ownership and expiry. Those checks do not qualify a target Kubernetes
store, its availability, wildcard DNS/TLS, or an ingress data plane. Validate these
and application requests in the deployment cluster.

Run the real local operator/CLI lifecycle suite with `make integration-previews`;
see [setup and coverage](preview-integration.md).
