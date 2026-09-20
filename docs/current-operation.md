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

Shutdown proofs describe the captured invocations. A concurrent ordinary
Kubernetes replacement can create a new ephemeral Bucket invocation that later
receives ordinary termination. Bucket mode requires object-store durability
before acknowledging writes; this is the same crash boundary as an unexpected
Pod loss. Persistent replacements use the existing PVC and cannot reopen a disk
with the launcher's permanent retirement marker. Neither case turns a new
invocation into one covered by an old proof.

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
capacity baseline (163,128 of 184,320 bytes). Maintenance clears pre-disruption
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
   Each PVC deletion uses its recorded UID and resource version, with current
   claim/PV/driver/handle and deletion-finalizer checks. Persist cleanup intent
   before deletion, then wait for PVC/PV absence and no matching attachment. A
   replacement claim, changed identity, existing Pod reference or pending CSI
   deletion blocks progress. The controller never removes storage finalizers.
7. For restart/upgrade, wait at zero replicas until captured disk cleanup completes, then use
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

PersistentFleet uses fresh dynamically provisioned RWOP disks under a supported
CSI class with `Delete` reclaim policy and `WaitForFirstConsumer`. StatefulSet
PVC retention stays `Retain` so only the controller initiates claim deletion.
Strict proof and observed compute removal precede UID/resource-version guarded
PVC deletion. Current cleanup binds the exact claim, PV, CSI driver and handle
and captures the external-provisioner deletion finalizer. Completion waits for
PVC/PV absence and no matching VolumeAttachment.

CSI's deletion finalizer supplies the backend-deletion guarantee; the operator
never removes finalizers, force-detaches disks or calls cloud APIs. It keeps
current proof while cleanup is pending and discards it only after completion.
There is no historical cleanup flag or deletion authority. Historical retained
PVs cannot be adopted or retroactively disposed. See
[disposable disks](disposable-disks.md) for the exact contract and validation.

Growth and coordinated maintenance use fresh claims. A disk-scoped launcher
marker permanently denies any later launch on a strictly stopped disk. A host or
boot change is refused before startup because local locks cannot prove exclusion
across kernels. There is no handoff grant or fencing override.

Provisioning requires an explicit `ghcr.io/ewhauser/celld@sha256:...` pin and a
digest-pinned launcher image. Pin syntax is not artifact qualification. All
recovery participants need the compatible fork's native `bucket_complete`
reader. Stock upstream v0.5.1 cannot satisfy the protocol. Native binary hashes
are not container digests; test pins are explicitly fixtures.

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
| Multi-member restart/upgrade/delete | `TestMaintenanceUsesCurrentWorkingSet`, `TestDeleteUsesStrictWorkingSetAndDisposesDisks`, `TestDeploymentBucketMaintenanceAndContractionBoundary` |
| Archive growth and editable status | `TestCurrentAuthorityBoundedAndStatusRebuildable`, `TestHundredMemberMaintenanceFitsBound` |
| Storage cleanup and stale deletion | `TestStrictRemovalBothProfiles`, `TestEnvtestCleanupPreconditionsRejectReplacement`, manifest RBAC audit |
| No private recovery reads | `TestNoRecoveryMetadataDependencies`; production manager no longer constructs S3 evidence or fencing clients |

The opt-in local integration runs the real fork binary, typed control-plane
client and launcher HTTP protocol against isolated MinIO. It persists proof in
the controller, stops the launcher, recreates the controller, then verifies the
guarded workload effect and simulated Kubernetes storage cleanup:

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
