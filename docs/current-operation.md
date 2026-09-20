# Bounded current operation

The operator now has one lifecycle executor. celld decides data safety through
its strict `remove-disk` control plane. The launcher binds that result to one
child, waits for its exit, independently reacquires the inherited lock and
persists restart denial. The operator serializes the resulting Kubernetes
changes. It does not inspect private S3 keys, runtime logs, epochs, follower
inventories or leases, and cannot substitute EC2 termination for celld completion.

This is a breaking replacement. There is no journal migration, old runtime
adapter fallback, archive hydration, retained-member replay or second executor.
The Bucket layout is immutable. Ordered Bucket supports single-member
contraction; Deployment victim selection cannot establish an exact target, so
Deployment contraction is blocked. Deployment restart/deletion can stop its
entire bounded current working set before changing replicas.

## Durable state

The storage reservation's immutable spec still owns the bucket and fleet UID.
Its `celld.eric.dev/current-operation` annotation contains:

- Current fleet/workload UIDs, applied replica count and runtime image, plus the
  current PVC UID bindings (at most 100).
- One operation ID, kind, phase and absolute deadline; source/target replica
  counts and images; the captured desired-request and capacity-policy identity.
- The current target, or at most 100 targets for coordinated maintenance. Each
  binds Pod UID, container incarnation, endpoint, host UID/boot, launcher
  invocation, runtime generation and disk nonce. Persistent targets also bind
  PVC/PV UIDs, names and the CSI volume handle.
- Only captured celld completion and launcher child-exit, inherited-lock-release
  and durable restart-denial evidence for those targets.
- The workload effect predecessor and resource version; exact-resource cleanup
  preconditions; bounded capacity-policy stabilization state.
- One current completed restart token and one last completion projection.

There is no append-only completion or session history. A normal completed
three-member PersistentFleet retains roughly 518 bytes in the fixture; the
100-member regression uses realistic UUIDs, 64-hex container/runtime identities,
PVC/PV UIDs and CSI handles and exercises all captured proofs plus the retained
capacity baseline (158,429 of 184,320 bytes). Maintenance clears pre-disruption
load samples while preserving the ineffective-addition hold. Encoded state has
a hard 180 KiB cap and rejects unknown fields and invalid operation shapes.
Oversized authority fails closed instead of creating an archive.

Status is rebuilt from these Kubernetes objects. Editing or erasing status
cannot create a proof, change an operation or authorize an effect.

## Issuance and effect protocol

1. Discover and recheck the exact target. Validate current storage ownership,
   workload configuration, placement and survivor capacity. Persist `Intent`
   and a fixed deadline without contacting the launcher to remove anything.
2. A reservation resource-version CAS advances to `Requesting`. Only this
   transition authorizes a strict launcher request. A competing cancellation
   can win only while the operation is still `Intent`.
3. Issue/retry the exact operation and generation with the original deadline.
   Shorter reconcile/HTTP timeouts do not replace that recorded deadline.
   After expiry, observe only. HTTP acceptance, readiness, elapsed time, absent
   Pods and exit codes cannot become completion. A failed launcher result is
   terminal; a later exit cannot repair it.
4. Validate the entire authenticated launcher response, including the exact
   operation, deadline, Pod, invocation, generation, host/boot and disk identity.
   Require both celld's terminal control-only `data_safe` result and every
   independent launcher proof. Recheck Kubernetes identity before saving it.
   Each current target's proof is persisted before moving on; completed proof
   survives controller or launcher restart without re-reading runtime logs.
5. Record an effect resource version only after validating the original workload
   UID, exact predecessor effect marker, source count/template and target disks.
   Apply that exact CAS with the operation's `/stop` marker. A conflict requires
   full revalidation and another reservation CAS; no blind fresh-version retry
   is allowed. The exact marker and resulting count reconstruct a lost response.
6. Observe the specified replica and Pod effects. Only then enter PVC cleanup.
   Each PVC deletion uses its recorded UID and resource version. A replacement
   claim, changed PV/handle, existing Pod reference, or pending PVC-protection
   finalizer blocks progress. The controller never removes storage finalizers.
7. For restart/upgrade, wait at zero replicas until old PVCs are gone, then use
   the same guarded effect protocol with `/resume`, the recorded image and fresh
   claim names. Admit new claim UIDs before releasing their scheduling gates.
   Complete only after the specified Pod count and workload readiness are
   observed. Discard the operation and its runtime proof at completion.

Once `Requesting` wins, pause, desired-count reversal, a new restart token and a
new upgrade request cannot cancel or retarget it. The existing operation must
finish first. Expiry never establishes that a shutdown was unissued. A missing
or ambiguous launcher result preserves the operation and disk indefinitely.
An old issuer cannot refresh itself onto a new effect: the reservation CAS and
the independently checked workload predecessor/UID/resource-version fence both
boundaries. Delayed cleanup carries the old PVC UID and cannot delete a new
claim that reuses its name.

## Storage and rollout boundaries

The StorageClass and PV contract remains **Retain**. Successful PVC cleanup is
successful compute removal and claim-name release, not EBS deletion. PVs and
physical disks remain retained. The `DiskCleanupPending` condition and last
completion make that distinction visible. Growth uses new PVCs; it never adopts
an accumulated retained-member record. Cross-host/boot reuse remains blocked by
the launcher, with no operator handoff grant or fencing override.

**After completion, the positive removal proof has been discarded.** The
remaining `DiskCleanupPending` boolean, a released Retain PV, its claimRef, and
the last completion are not authority for automatic historical PV/EBS deletion.
Item #6 must establish its own current exact-resource deletion contract and
real CSI/EBS qualification; it must not infer safety from these remnants or
rebuild a history archive. This implementation never deletes a PV, changes a
reclaim policy, force-removes an attachment or terminates an EC2 instance.

There is no default compatible image. Provisioning requires an explicit
`ghcr.io/ewhauser/celld@sha256:...` pin and a digest-pinned launcher image.
Syntactic pin validation is not artifact qualification. A homogeneous compatible
fork, including recovery readers for native `bucket_complete`, must be qualified
before rollout. Stock upstream v0.5.1 cannot provide this contract. No image
digest is invented from a native-binary artifact. Tests label synthetic pins as
fixtures.

## Regression and qualification coverage

The old journal-layout, S3-session, archive and infrastructure-fencing tests were
replaced at their active behavior boundaries:

| Former concern | Current regression |
| --- | --- |
| Crash before/after intent, issuance, proof and effects | `TestLostResponsesAndControllerRestarts` |
| Stale issuer and cancellation/workload CAS | `TestStaleReservationAndWorkloadWriters`, `TestEnvtestBoundedOperationCASAndLostResponse` |
| Acceptance, exit status, readiness or absence mistaken for safety | `TestRemovalRejectsIncompleteAndWrongProof`, `TestNoRuntimeProofFromAbsentPodOrReadiness` |
| Failed capture later promoted by exit | `TestRemovalRejectsIncompleteAndWrongProof/failed` |
| Launcher crash before/after Kubernetes capture | `TestLauncherCrashCaptureBoundary` |
| Pause, reversal, concurrent restart or upgrade | `TestCancellationReversalPauseAndNewRequests`, `TestDeadlineNeverMakesIssuanceUnissued` |
| PVC/PV/workload/runtime replacement | `TestIdentityDriftBlocksProofAndEffect`, `TestStateRejectsUnboundOrOversizedAuthority` |
| Retained-disk reactivation | `TestCurrentAuthorityBoundedAndStatusRebuildable` (fresh claims), strict launcher cross-host/boot tests |
| Capacity/manual/external entry points | `TestCapacityEntriesUseStrictCurrentOperation`, `TestCapacityCollectionEditAndRevalidation` |
| Multi-member restart/upgrade/delete | `TestMaintenanceUsesCurrentWorkingSet`, `TestDeleteUsesStrictWorkingSetAndRetainsDisks`, `TestDeploymentBucketMaintenanceAndContractionBoundary` |
| Archive growth and editable status | `TestCurrentAuthorityBoundedAndStatusRebuildable`, `TestHundredMemberMaintenanceFitsBound` |
| Storage cleanup and stale deletion | `TestStrictRemovalBothProfiles`, `TestEnvtestCleanupPreconditionsRejectReplacement`, manifest RBAC audit |
| No private recovery reads | `TestNoRecoveryMetadataDependencies`; production manager no longer constructs S3 evidence or fencing clients |

The opt-in local integration runs the real fork binary, typed control-plane
client and launcher HTTP protocol against isolated MinIO. It persists proof in
the controller, stops the launcher, recreates the controller, then verifies the
guarded workload/PVC effect and retained PV:

```sh
CELLD_STRICT_TEST_BINARY=/absolute/path/to/strict/celld \
CELLD_STRICT_TEST_ESBUILD=/absolute/path/to/esbuild \
go test -race ./internal/controller -run '^TestStrictRuntimeCurrentOperation$' -v -count=1
```

The September 20 run passed with the verified `0.5.1-ewhauser.1` macOS ARM64
binary, SHA256 `a295e40f971e9800164b33e1e8e6e13f5d96ead32a210481f348bba35223c785`.
It uses an empty runtime disk and simulated Kubernetes/CSI resources. Envtest
separately exercises a real API server and etcd. Neither qualifies replicated
workload recovery, Kind's full workload controllers, EKS/EBS or real disk cleanup.

Item #5 still owns repository-wide historical documentation, old offline v050
qualification adapters/tools and old Kind scenario expectations. The active
controller and manager have no references to them. Item #6 owns the disposable
disk policy and real CSI/EBS qualification described above.
