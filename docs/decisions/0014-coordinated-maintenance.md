# ADR 0014 Coordinated maintenance and retained deletion

Implementation follow-up: [maintenance execution](../maintenance-execution.md) supersedes the blocked-only restart/deletion behavior below for admitted Bucket and launcher-managed PersistentFleet operations. Different-version transitions remain unsupported; PersistentFleet final shutdown requires positively sealed logs for every stopped leader.

Status: Implemented blocked disruption requests and maintenance fencing; no runtime transition qualified

This extends ADRs 0012 and 0013. The lifecycle reservation remains the only
operation journal. There is one executable `Operation`, shared by manual and
automatic capacity. Upgrade, restart and deletion requests occupy `Request` in
that same journal; they never authorize a separate workload writer. There is no
Pod deletion, eviction, template update, rollout annotation, storage cleanup or
reservation release path. RBAC intentionally retains no delete privileges.
(Superseded by the later maintenance and Bucket migration executors, which delete
pods and workloads with UID preconditions; those verbs are granted only inside
fleet namespaces by `config/rbac/fleet-namespace.yaml`, never cluster-wide.)

## Runtime transitions

`spec.runtimeImage` is an optional immutable celld digest. Omission selects the
original v0.5.0 OCI index pin. Explicitly selecting that exact pin is a no-op,
not a restart. Tags and other repositories fail validation. A syntactically
valid different digest is accepted as a request and reports UnsupportedTransition;
it is never pulled or applied. Initial provisioning likewise rejects another pin.

The qualified transition matrix is **empty**, in both directions and both
profiles. This includes rollback, stopped-fleet migration and switching from the
index to an architecture-specific digest. Adapter support for decoding a digest
is not transition qualification. The v0.4.1 to v0.5.0 stopped-fleet procedure has
not been qualified here; reversing it is not implicitly supported.

The journal writes version 2 so step-4 binaries reject it rather than ignoring
maintenance fields; operator downgrade over a version-2 journal is unsupported.
Version-1 journals are read conservatively and rewritten by the next journal CAS.
The journal records the applied runtime pin. Legacy version-1 journals lacking
that field mean exactly the original pin; any other applied pin is rejected.
Recovery selects the v0.5.0 adapter from this applied identity, never from the
requested target. A request records UUID, kind, source/target pins, restart token,
fleet generation and workload UID. No Pod target is fabricated: target Pod UID,
container/session inventory and process fence must be captured and persisted in
Operation before a future qualified executor may issue a disruption.

## Ordering and edits

An existing capacity operation retains its original target through runtime and
restart edits. After it completes, a maintenance request takes precedence over
new manual/automatic capacity. A blocked request may be replaced or withdrawn
because no effect was authorized. Its UUID stays stable across reconciles and
leader restarts while its kind, target image and token are unchanged. Clearing
fleet status does not clear requests, sessions, loss findings or claims.

`spec.maintenance.restartToken` is an optional, at-most-128-character token.
A nonempty token requests restart and currently reports DisruptionUnqualified.
Removing it withdraws the blocked request. There is no successful-restart ledger
because no restart is permitted. An image request takes precedence over restart;
deletion takes precedence over both. These precedence rules do not retarget an
already recorded capacity operation.

## Pause and concurrency

`spec.maintenance.paused` defaults effectively to false. True stops new initial
provisioning, new lifecycle/capacity decisions and unissued replica effects.
Prerequisite creation and capacity collection are suspended. A recorded but
unissued addition or removal remains frozen with its original operation UUID,
target and claims. Resume revalidates infrastructure, claims and the original
operation; it does not implicitly cancel or change it. Additions finish their
original target after resume. Automatic removals still need a new complete
low-demand window and all existing recovery gates.

Pause invalidates cached actionable decisions and positive capacity windows.
It never turns an intermediate, contrary, missing or repeated observation into
a positive sample. Source watermarks and action history remain retained.

For an existing owned workload the controller CASes `maintenance-fence=paused`
onto workload metadata, the same object/version used for replica issuance.
This is not a template change or a runtime process fence. If this write wins,
a delayed old issuer loses its CAS. If replica issuance wins first, it is already
issued: additions are recorded complete without repeating the effect; removals
continue exact-session recovery even while paused. No new operation can start.
Pause does not interrupt celld, remove Service routing or prevent Kubernetes from
restarting failed Pods. In-flight exclusive PVC allocations may finish and are
retained; they cannot make a fenced replica write succeed.

The spec edit is an asynchronous request, not an atomic cancellation transaction.
A controller may already have accepted initial creation intent or issued a replica
write before acknowledging pause. Initial creation already past its durable
attempt can finish. Where the workload is absent, pause reports that fact rather
than claiming a process fence. Resume removes only the maintenance marker after
rereading fleet UID and control state, using the workload CAS, then waits for the
next reconcile to revalidate. A delayed resume cannot overwrite an acknowledged
newer workload fence. The loss fence is independent and is never cleared.

## Finalizer-driven deletion and garbage collection

Deletion enters the same maintenance path with a `deleting` fence, freezes
unissued work and persists a Delete request when the journal/workload can be
validated. Already-issued recovery continues and still records sticky loss or
uncertainty. Final shutdown has no qualified executor. Therefore DeletionBlocked
retains the finalizer indefinitely; it never scales to zero, deletes a workload
or falsely declares the fleet deleted. Missing workload/reservation evidence also
blocks; absence is not proof that old processes stopped. No force-complete switch
or timeout is supplied, including for partially provisioned fleets.

Workloads, prerequisites, PVCs and the cluster-scoped storage reservation have no
fleet owner references. Foreground/background CR deletion cannot garbage collect
them through the fleet. StatefulSet retention is Retain for deletion and scaling;
the StorageClass must use Retain reclaim policy. Lifecycle rejects added owner
references on reservations/workloads/retained claims and changed claim bindings.
Reservation UID ownership, original immutable spec hash, creation attempt and PVC
UID inventory remain, together with all operation/session/loss history. Neither
new fleet UID nor same-name recreation may reuse that bucket or retained disks.

Namespace deletion or direct privileged deletion can still destroy namespaced
Kubernetes evidence/workloads. This operator cannot promise data survival against
those administrative actions. The cluster-scoped reservation remains a permanent
bucket tombstone, preventing silent reuse; it is never owned by the namespace.
Storage records and disks must be retained externally, including S3 node/loss
records with no expiration. Manual removal of finalizers or reservation deletion
is outside the supported workflow and is not a cleanup instruction.

## Remaining gates

Before any executor can be enabled: qualify both source and target runtime's
peer/storage schemas and mixed-version or stopped-fleet procedure, directional
rollback, exact process fencing, complete fresh live and historical session
inventory, read-only S3 transport/IAM and loss discovery, retained-EBS identities,
lease-aware restart and no stale writers. Qualify crash points before/after intent,
issue and completion, repeated disruption with an acknowledged-write ledger, final
fleet shutdown with no survivor and safe finalizer completion. Bucket additionally
needs a no-log completion rule and deterministic victim control. Current local
runtime results, synthetic controller tests and kind are not EKS/S3/EBS production
qualification. Follower AZ diversity and sustained pressure relief remain open.
