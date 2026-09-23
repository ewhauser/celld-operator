# Preview state seeding

A `CelldPreview` can request a one-time copy of multiple Durable Objects before
its runtime starts. The operator authorizes the source and stores the initialization
request and executor result on the preview fleet's existing, retained
`CelldStorageReservation`. **No additional pool or seed CRD is required.**

This repository implements operator orchestration only. It includes neither a
snapshot/import executor nor a `celld preview` CLI. With no executor, a seeded
preview stays `Initializing` and never starts a runtime.

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
  fleetRef:
    name: development
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

`seed.source` is an administrator-approved alias, not an arbitrary fleet or bucket
address. Selections contain 1–100 distinct class/ID pairs. IDs must be canonical
runtime object IDs; a future CLI may resolve human-readable names first. Preserve
the class/binding mapping in the preview application. Seeding, including its
presence/absence, is fixed at creation. Updating `revision` never reseeds.

Snapshots are consistent per object, not globally across the selection. Copy
persisted SQLite/KV state only. Heaps, sockets, in-flight requests, production
credentials and external service data are excluded. `alarms: Clear` is the default;
`Preserve` explicitly requests restoring scheduled work. The executor must
implement these semantics before claiming compatibility with this protocol.

## Platform authorization

Configure seeding under the existing parent fleet's `spec.previews`:

```yaml
spec:
  previews:
    # Existing preview runtimeImage, storage, routing, etc.
    seeding:
      executor: celld-snapshot-v1
      sources:
        - name: production
          fleetRef:
            namespace: production
            name: my-app
            uid: replace-with-the-source-fleet-kubernetes-uid
```

See [the fleet configuration sample](../config/samples/fleet-previews.yaml).
The exact source UID is mandatory: replacing a source name never transfers this
grant. The controller checks the live source UID and permanent storage reservation
before creating the child. The executor rechecks authority before exporting.
Anyone allowed to create previews against this parent can select objects from
these sources; use separate parent configurations/namespaces for different policies.
The preview bucket must be separate from the parent's runtime bucket.

Install the three CRDs (`CelldFleet`, `CelldPreview`, `CelldStorageReservation`)
and updated operator RBAC. Developers must not have child-fleet, reservation or
reservation-status write access. [Executor RBAC](../config/rbac/preview-seed-executor.yaml)
is an optional, unbound example for a separately installed trusted executor.
Reservations are cluster-scoped, so its status permission is also cluster-scoped;
restrict `resourceNames` for a fixed set where appropriate. Status access cannot
change immutable requests or operator-owned lifecycle/startup annotations.
Supply source/target storage access and private connectivity separately. The
operator never reads or copies object-store credentials or snapshot bytes.

## Durable initialization and startup

The child's immutable `storage.initialization` contains the executor name, source
fleet identity, selection, alarm policy, target preview UID/name, target fleet name,
parent fleet UID, destination URL and creation-based deadline. The controller copies
this request into the destination reservation's immutable `spec.initialization`,
alongside the actual target fleet UID and configuration hash. No second operation
resource is created. Preview status is a projection of this retained record:
`status.seedReservation` identifies it and `status.seedPhase` reports progress.

Before creating the reservation, the operator records intent on the child and then
pins the reservation UID. An ambiguous creation with no surviving record blocks;
an accepted creation whose response was lost can be resumed with the same UID.
A lost or replaced reservation is never silently recreated. Preview creation has
a corresponding child-creation intent. Changing code does not change either identity.

The reservation's status contains `phase`, `canceled`, `executorID`, `targetFleetUID`,
`manifest` and a bounded `message`. API admission pins execution identities and
snapshot manifests, freezes terminal results, prevents cancellation reversal and
requires a prior Running claim for success. The operator additionally checks that
success covers every selected object exactly once and matches the child UID.

Before provisioning, the operator records the preview UID and canonical manifest
digest in the reservation's `celld.eric.dev/seed-gate` annotation using
resourceVersion concurrency control. No runtime, Service or route is created
before this gate opens. The existing fleet journal governs normal startup and
shutdown after that point. Preview status loss and application revisions do not
reinitialize the destination.

## Executor protocol

A conforming executor must:

1. Watch cluster-scoped reservations with `spec.initialization.executor` matching
   its implementation. Resolve the destination namespace from `spec.fleetNamespace`.
   Verify the live preview and child exist and are not deleting; check preview
   ownership, parent storage authority, the request, pinned reservation UID,
   fleet configuration and destination prefix. Require an unset seed gate and
   no workload creation intent or lifecycle journal. Recheck source authority.
2. Before any I/O, atomically claim `status.phase: Running`, `status.executorID`
   and `status.targetFleetUID` using the observed resourceVersion. Require an
   uncanceled, unexpired request in Pending/unset phase. A competing worker must
   not process Running work. Resume only after proving exclusion of a former
   writer; generic overlapping Job retries are insufficient.
3. Capture durable, consistent snapshots of every selected object. Publish the
   complete `status.manifest` before importing. Each entry contains `class`, `id`,
   immutable `snapshotID`, committed `sourceVersion` and payload SHA-256 `digest`.
   Persist snapshot bytes independently of worker lifetime. Use the reservation
   UID as an idempotency key so an uncertain capture cannot select different data.
4. Validate snapshot hashes and source/target format compatibility. Import only
   those pinned snapshots using the reservation UID as the idempotency key and
   the requested alarm policy. Resume partial imports into the unopened destination.
   Never copy source leases, node ownership, replication journals or process metadata.
   Finish and stop all writers before setting `Succeeded`.
5. Observe `status.canceled` and the deadline throughout capture/import. Stop writes
   and prevent delayed retries before acknowledging `Canceled` or terminal `Failed`.
   Never report success after cancellation. Preserve the manifest for diagnosis.

Example terminal reservation status (one object shown for brevity):

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

The operator validates identity and completeness, not snapshot bytes or storage-level
proof. Runtime consistency, import idempotency and writer exclusion still require
implementation and qualification. `Ready` confirms infrastructure only; callers
must deploy and verify application code. Do not mutate destination object state
while initialization runs.

## Cancellation and retention

TTL includes initialization time. Expiry or deletion cancels initialization before
deleting the child. An unclaimed request can be atomically canceled by the operator.
A Running request must be acknowledged by its executor; an unavailable executor
leaves the preview Deleting. Claim and cancellation use the same resourceVersion.

A never-started child releases its finalizer only after the executor is terminal,
a permanent canceled gate is recorded, and no workload creation intent, lifecycle
journal or workload exists. Startup and cancellation compete on the same retained
reservation. If startup wins, normal fleet shutdown safety applies. Direct child
deletion also cancels initialization.

The cluster-scoped destination reservation, manifest and partial object data are
retained after preview/child deletion. Lost records block recovery; never clear
intent annotations or reset terminal state to retry. Create another preview.
No snapshot or object-data garbage collector is included.
