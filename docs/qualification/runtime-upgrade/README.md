# Local released-image upgrade qualification

Run: September 18, 2026 local time (September 19 UTC), macOS/Colima Docker,
MinIO, two PersistentFleet celld processes and retained Docker volumes. No cloud
resources, Kubernetes context, custom celld build or upstream changes.

Command:

```
.qualification-venv/bin/python hack/qualification/versions.py .qualification-runs/versions/live7
```

The 420-second bounded experiment completed successfully and removed all owned
containers, volumes and its Docker network. The exact canonical release digests,
OCI revisions, architecture and harness hashes are in `provenance.json`.

Observed sequence:

1. Start unchanged v0.4.1 processes a and b; both become healthy.
2. Write 12 operations and record only successful acknowledgments.
3. Stop a, leave b alive until its recovery positively seals a's exact generation,
   then stop b. Require both original logs sealed and all source leases expired.
4. Start unchanged v0.5.0 on the same retained volumes. Both become healthy with
   different ownership generations; no loss objects appear in the observed log inventory.
5. Read all 12 original operations through b. Write six new operations through b,
   then read all 18 through a. Missing operations: zero.

`docker.log` and `result.json` contain bounded result evidence. Actual v0.4.1
private-state and node-record samples are retained as codec regression fixtures
in `internal/runtime/catalog/testdata/`.

This is a concrete forward runtime/storage compatibility test. It is not a live
Kubernetes operator upgrade or EKS/EBS qualification, rolling upgrade, arbitrary
version or rollback qualification. The fixture explicitly waits for a live peer
to seal the first stopped source before stopping the last peer. The coordinated
operator currently stops every member before its all-sealed gate and therefore
can remain safely blocked on a busy source fleet with unsealed logs. Its completed
all-sealed transition, crash/lost-response recovery, exact image authority,
new generations and claim retention are tested with fake Kubernetes/runtime
clients. Unsealed full-stop recovery needs a separately qualified protocol;
no successful live controller upgrade is claimed here.

During development, actual images exposed two distinct configuration contracts:
v0.4.1 requires its drain-token timeout within the shutdown budget; v0.5.0 rejects
that removed setting. The operator installs the version-specific container
configuration in the same zero-replica CAS as the target image.
