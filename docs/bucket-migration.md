# Deployment-to-Ordered Bucket migration

Existing v0.5.0 Bucket fleets can explicitly migrate to the Ordered StatefulSet
layout. This is a one-way operation with whole-fleet downtime, not a rolling
update. It does not convert PersistentFleet storage or change the runtime image.

After the fleet has a stable lifecycle journal and no active capacity,
maintenance, or runtime-transition operation, update these fields together:

```yaml
spec:
  bucketWorkload: Ordered
  maintenance:
    allowCoordinatedDowntime: true
    orderedMigrationToken: ordered-layout-1
```

The operator retains the original reservation hash, fleet UID, bucket, complete
writer history, and source workload UID. It captures the exact admitted Bucket
sessions, fences delayed replica mutations with a workload CAS, reduces the old
Deployment to zero, and requires positive expiry/no-log evidence for every
captured writer. Missing metadata before positive expiry remains a blocker.

Only after stable recovery evidence and foreground removal of the old Deployment
may it create a zero-replica Ordered StatefulSet. Its exact UID is journaled
before a separate UID/resourceVersion-guarded activation sets the captured replica
count. A delayed old Create can therefore install only an inert workload.
The new workload receives the recorded migration operation ID, a new workload
UID, and fresh Pod UID runtime identities.
A retained creation authorization makes an interrupted Create recoverable without
adopting an unrelated StatefulSet. Existing foreign target workloads are refused.
The original reservation remains bound to its original Deployment configuration;
the durable migration journal authorizes the sole layout/UID transition.

Pausing before admission prevents new disruption. After admission, recovery
continues through the captured replacement, including while paused. Deleting the
fleet mid-migration retires the source and retains data; an already-created inert
target is removed with UID/resourceVersion guards.
If activation was already issued, it completes that identity transition before
the ordinary final shutdown path. Delayed old Creates cannot install running
workloads after the finalizer is removed.

The migration is complete when the journal records the new workload UID. Normal
provisioning then verifies scheduling and readiness; completion of the identity
transition is not a claim that every replacement Pod is already Ready. Automatic
capacity stabilization resets. Strict AZ scheduling uses the same ordinal-zone
rules as a newly created Ordered fleet.

Local regression tests cover durable phases, positive expiry, identity/history
retention, and interruption boundaries. Live cluster qualification remains
outstanding. See [runtime dependencies](runtime-dependencies.md) for the separate
legacy unwrapped/RWO storage conversion and Bucket GC limitations.
