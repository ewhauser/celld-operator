# Preview state seeding

A `CelldPreview` can request a one-time copy of multiple Durable Objects before
its runtime starts. The operator authorizes the source, creates a retained
`CelldPreviewSeed` request, reserves the destination prefix and gates workload
creation and routing. A separate, trusted celld executor must capture and import
the actual snapshots. **This repository implements the operator side only. No
snapshot/import executor or `celld preview` CLI is bundled.** With no executor,
a seeded preview stays `Initializing` and never starts a runtime.

## Developer API

The proposed CLI can translate repeated `--object` selections or a fixture file
into [this manifest](../config/samples/preview-seeded.yaml):

```yaml
apiVersion: celld.eric.dev/v1alpha1
kind: CelldPreview
metadata:
  name: reproduce-checkout
  namespace: previews
spec:
  poolRef:
    name: shared-previews
  source: feature/checkout-fix
  revision: abc123
  ttlSeconds: 86400
  seed:
    source: production
    alarms: Clear
    objects:
      - class: Cart
        id: cart-123
      - class: Customer
        id: customer-456
      - class: Inventory
        id: sku-789
```

`seed.source` is a pool-approved alias, not an arbitrary fleet or bucket address.
Selections contain 1–100 distinct class/ID pairs. IDs must be canonical runtime
object IDs; a future CLI may resolve human-readable names before submitting the
request. Preserve the class/binding mapping in the preview application so lookups
reach those identities. Seeding is immutable, including its presence/absence;
it cannot be added to a running preview or rerun by updating `revision`.

Snapshots are consistent per object, not globally across multiple objects.
Copy persisted SQLite/KV state only. Execution starts fresh: heaps, sockets,
in-flight requests, production credentials and external service data are not
part of the seed. `alarms: Clear` is the default; `Preserve` explicitly requests
restoring scheduled work. The executor must implement these semantics before it
can claim compatibility with this protocol.

## Platform authorization

Add this block when creating the otherwise unchanged
[preview pool](../config/samples/preview-pool.yaml):

```yaml
spec:
  # Existing runtimeImage, storage, routing, etc.
  seeding:
    executor: celld-snapshot-v1
    sources:
      - name: production
        fleetRef:
          namespace: production
          name: my-app
          uid: replace-with-the-source-fleet-kubernetes-uid
```

The source UID is mandatory. A replaced source fleet never inherits this grant.
The operator checks the live source UID and permanent storage reservation before
creating the seed request. Pool configuration is immutable. Anyone allowed to
create previews against this pool can request selected objects from these
sources; use distinct pools/namespaces to separate data-access policies.

Install all five CRDs and the updated controller/namespace RBAC. The operator
can create/patch seed requests and cancel unclaimed requests through status.
Developers should have no seed status, child fleet, or reservation write access.
[Executor RBAC](../config/rbac/preview-seed-executor.yaml) is an optional example,
not installed or bound automatically. Supply source/target storage access and
private runtime connectivity separately to the trusted executor. The controller
never reads or copies object-store credentials, snapshots or application data.

## Reconciliation and startup

The operator creates one retained `CelldPreviewSeed` per preview UID. Its immutable
request contains the executor name, exact source fleet identity, object selection,
alarm policy, target preview UID, target fleet name, pool UID, destination URL and
creation-based expiry deadline. A creation-intent marker prevents ambiguous
failures or lost requests from producing a replacement operation.

The child fleet carries an immutable `storage.initialization` reference to the
seed request's exact name and UID. It acquires its permanent prefix reservation
before the executor can write. It creates no runtime, Service or public route
until the gate opens. Seeding does not use replica scaling to zero, so it does not
bypass the normal fleet lifecycle contract.

Only a successful result for the exact target fleet UID, containing snapshots for
**every** selected object and no extra/duplicate entries, can open the gate. The
operator stores the seed UID and snapshot-manifest digest on the permanent
reservation using resource-version concurrency control before provisioning.
This receipt survives ordinary preview status loss and manager restarts. A new
application revision never reseeds the destination. A missing/replaced seed
request is reported as blocked, not recreated; an already-initialized fleet's
own lifecycle can still use its retained receipt.

`status.seedName` and `status.seedPhase` expose the operation. Preview phase stays
`Initializing` while waiting, becomes `Blocked` on terminal seed failure, and
continues normal provisioning after success. As for empty previews, `Ready`
confirms infrastructure only; the CLI must also upload and verify application
code. The CLI must not mutate the target object state while initialization runs.

## Executor protocol

The external executor is a trusted storage writer. Status is a durable work
record with admission-enforced immutable claims, snapshot manifests and terminal
results; the preview's status is only a projection. A conforming executor must:

1. Watch requests matching its configured `spec.request.executor`. Before claiming,
   verify the request is uncanceled and unexpired, the preview exists and is not
   deleting, and the child fleet's controller owner and initialization reference
   match the request's preview UID and seed UID. Resolve the target namespace
   from the seed resource's namespace. Confirm the source UID/configuration and
   storage authority again; never trust a source name alone.
2. Wait for the target prefix reservation to bind the actual child fleet UID,
   immutable configuration, bucket, prefix and endpoint. Verify the pool's root
   reservation too. Require the destination seed gate to be unset, with no
   workload creation intent or lifecycle journal. Do not write without this
   reservation, even though the target URL is already visible in preview status.
3. Atomically claim `status.phase: Running`, `status.executorID` and
   `status.targetFleetUID` using the observed resourceVersion. Exactly one worker
   may claim Pending/unset status. A competing worker must not process a Running
   request. The same durable execution may resume only after proving exclusion
   of its former writer; generic overlapping Job retries are insufficient.
4. Capture consistent, durable snapshots of all selected objects. Publish one
   complete `status.manifest` before importing any object. Each entry includes
   `class`, `id`, opaque immutable `snapshotID`, committed `sourceVersion` and the
   exported payload's SHA-256 `digest`. Persist snapshot bytes independently of
   worker lifetime. Admission forbids changing or removing the pinned manifest,
   including before completion. Capture recovery must use the request UID as an
   idempotency key so an uncertain capture response cannot select different data.
5. Import using those pinned snapshots, the request UID as an idempotency key,
   and the selected alarm policy. Validate snapshot hashes and source/target format
   compatibility. Retries must resume partial imports into this reserved, unopened
   destination. Do not copy source leases, node ownership, replication journals or
   process metadata. Finish and stop **all** writers before setting `Succeeded`.
6. Observe cancellation/deadline throughout capture/import. Stop all writes and
   prevent delayed retries before reporting `Canceled` or terminal `Failed`.
   Never report success after cancellation. Retain the manifest for diagnosis.

Example terminal result (abbreviated to one object for illustration):

```yaml
status:
  phase: Succeeded
  executorID: seed-execution-unique-id
  targetFleetUID: actual-destination-fleet-uid
  manifest:
    objects:
      - class: Cart
        id: cart-123
        snapshotID: immutable-snapshot-handle
        sourceVersion: committed-version-42
        digest: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

The operator checks identities and manifest completeness; it does not independently
verify snapshot bytes or turn a worker's status into a storage-level proof. The
executor's snapshot consistency, import idempotency and exclusion guarantees need
runtime implementation and qualification before enabling real cloning.

## Cancellation, failures and retention

TTL includes initialization time and is not renewed by deployments. Expiry or
preview deletion requests cancellation before deleting the child. An unclaimed
request can be atomically canceled by the operator. A Running request must be
acknowledged by its executor; executor unavailability leaves the preview Deleting.
An optimistic-concurrency race between claim and cancellation has one winner.

A never-started child can release its finalizer only after the executor is terminal,
a permanent canceled startup gate is recorded, and no workload creation intent,
lifecycle journal or workload exists. Startup and cancellation compete on the
same reservation resourceVersion. Once the ready gate opens, ordinary fleet
shutdown safety applies, including existing incomplete-provisioning recovery
constraints. Deleting the child directly also requests seed cancellation.

Failed, canceled and partial seed data is retained in the preview's exclusive
prefix. Seed requests and their manifests have no owner references and are
retained separately from the preview. Namespace deletion remains an administrator
operation and can remove namespaced records; reservations remain cluster-scoped.
Never delete/recreate an operation, clear intent annotations, or reset terminal
status to retry. Create a new preview instead. No snapshot or object-data garbage
collector is included.
