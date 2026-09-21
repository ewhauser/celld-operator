# Runtime upgrade qualification

**PASS:** both Bucket and PersistentFleet completed a real
`0.5.1-ewhauser.2 → 0.5.1-ewhauser.3` upgrade on 2026-09-21.
The maintenance suite exited zero. The operator and launcher were built from
exact source `010aca2aa797f868cbd9ceca44e7e50dcb564fe1`; the runtime executed as
native Linux arm64 in the disposable Kind cluster `celld-strict-507fb9f1`.

```sh
CELLD_RUNTIME_IMAGE=ghcr.io/ewhauser/celld@sha256:a00da2bcaeaee6879d658477cd1bdb354a5de55fa9e7f0ab5e2fd95e6e0ce080 \
CELLD_UPGRADE_IMAGE=ghcr.io/ewhauser/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29 \
make integration-maintenance
```

The [event log](events.log) includes the command, source, every test marker and
final cleanup. [result.json](result.json) records runtime sources, exact image
identities and checksums for all raw evidence. The [identity receipt](identities.json)
compares healthy pre-upgrade and post-upgrade Pod and disk snapshots.

Each fleet wrote twelve unique acknowledged IDs across twelve cells. Every ID
and stored value matched after coordinated restart and again after upgrade.
Restart completed across controller replacement; another controller replacement
did not replay the completed token. Upgrade required the new image on every Pod
and disjoint old/new Pod UIDs. PersistentFleet additionally required both old PVs
to disappear, no VolumeAttachments to reference them, and fresh PVC UIDs, PV UIDs
and CSI handles. Final deletion removed compute and storage while retaining each
bucket reservation. The owned cluster and both read-only observers exited cleanly.

This used Kubernetes v1.31.4, Calico v3.29.3, MinIO with a bounded 2 GiB
memory-backed volume, and distributed hostpath CSI with external-provisioner
v6.3.0, Delete/WFFC and ReadWriteOncePod. The observer confirmed the external
provisioner's deletion finalizer on both generations of PersistentFleet PVs.
Hostpath CSI has no attach operation, so absence of VolumeAttachments does not
qualify cloud detach behavior.

## Environment failure preserved

The preceding local `.3` faults attempt, cluster `celld-strict-a4b7efd8`, failed
before any celld runtime started. All four launcher init containers reported
`no space left on device` while copying the launcher. The harness timed out
waiting for initial readiness and cleaned up its owned cluster. That attempt
provides **no fault-scenario result**.

Only this task's completed amd64 image smoke artifact and identified unused
operator/launcher build-cache entries were removed. Free Docker disk space rose
from 7.3 to 8.4 GiB before the successful upgrade run. No unrelated containers,
images or caches were pruned. The successful run required no intervention;
read-only observers captured storage pressure and resource transitions. Its raw
log, the failed log and observer outputs are archived with SHA256 receipts in
[result.json](result.json), under:

`/Users/ewhauser/.codex/artifacts/celld-operator/strict-control-plane/2026-09-21/ewhauser3-local-004747Z/`

## Scope

Only base isolation and graceful maintenance ran here. The old `.2` runtime was
not subjected to its known lease-loss recovery failure. Hosted `.3` fault and
other suite results are separate evidence. This closes the previously unrun
actual different-runtime upgrade gate; it does not qualify AWS/EBS, EC2 loss,
cross-host or cross-boot disk recovery, MinIO Pod replacement, or a published
operator image. See the [runtime startup-recovery evidence](../native-peer-startup/README.md)
for the distinct `.2` recovery defect and `.3` fix.
