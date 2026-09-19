# Maintenance execution: local evidence and remaining qualification

The maintenance implementation has focused controller tests for:

- Exact-UID Bucket restart, positive lease expiry, same-UID generation successor
  admission before deletion, deadline/health revalidation, pause, token replay,
  and actual-target AZ placement.
- Bucket final shutdown/finalizer completion with permanent reservation retention.
- PersistentFleet restart with the departing-follower barrier and exact retained
  volume continuity; fresh survivor checks before issuing a stop.
- PersistentFleet all-stopped assessment: all own logs sealed, no missing or
  rewound metadata, no historical loss, and the narrowly scoped no-own-log case.
- Signed resurrection-denial receipts, legacy receipt rejection, and deletion
  before provisioning with a permanent tombstone.

Focused maintenance race tests passed. These controller tests use synthetic
runtime and Kubernetes evidence; they do not establish real-runtime or AWS
maintenance qualification.

## Bounded kind attempt

The live command `python3 hack/integration/run.py --maintenance` created only
`celld-step2-cf04b361`. Its **first runtime image pull** timed out after 300 seconds:

```
docker exec celld-step2-cf04b361-control-plane crictl pull ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8
```

A read-only inspection showed an active layer at 22.02 MB after approximately
four minutes. This was a slow external image download, before deployment of the
operator or celld runtime. **No live restart, shutdown, acknowledgement recovery,
or finalizer assertion ran.** The attempt is a qualification blocker, not a pass.

The harness removed its owned cluster, and the wrapper restored Docker VM
`fs.inotify.max_user_instances` from the temporary 1024 to its original 128.
A subsequent Docker container listing found no containers for that cluster.
The [bounded log excerpt](kind-timeout.log) records the timeout and cleanup;
unrelated Kubernetes resource dumps are omitted.

No second cold-cluster attempt was made. A future run should preserve bounded
failure diagnostics from launch:

```sh
CELLD_TEST_DIAGNOSTIC_HOLD_SECONDS=600 make integration-maintenance
```

The helper will exercise Bucket and launcher-managed PersistentFleet restart,
operator replacement, completed-token replay, acknowledged application data,
retained PVC identities, and final deletion. Its diagnostic hold does not bypass
any pinned-image or lifecycle safety check. EKS/S3/EBS qualification remains
separate, including cross-node attachment and fault behavior.

## Remaining implementation boundaries

- Different-version upgrade/rollback needs a second supported adapter and an
  explicit directional compatibility contract. Only same-pin restart executes.
- PersistentFleet removals/restarts retain at least two survivors: the pinned
  runtime internally tiers when its final follower leaves, but does not publish
  a replacement ensemble/epoch when no peers remain. The operator lacks an
  externally visible completion barrier for 2-to-1 contraction.
- Final PersistentFleet shutdown can finish only with positively proven seals
  (or proven never-had-an-own-log sessions) and exact stopped receipts. Timeout,
  missing evidence or unsealed logs retain compute/finalizer/data; automatic
  recovery from an all-stopped unsealed fleet is not implemented.
- Migration from an unwrapped workload or legacy RWO PVCs is not silently
  performed. A separately specified exclusive migration remains required.
