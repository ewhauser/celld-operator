# Disposable persistent disks

`PersistentFleet` uses fresh CSI disks and disposes them after a strict shutdown.
The required StorageClass has `reclaimPolicy: Delete`,
`volumeBindingMode: WaitForFirstConsumer`, and the `ebs.csi.aws.com` provisioner.
Local qualification permits `hostpath.csi.k8s.io` with the same contract. There
is no Retain mode, adoption of existing claims, or migration from older operator
state. The bucket reservation remains permanent after fleet deletion.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ebs-delete
provisioner: ebs.csi.aws.com
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
parameters:
  type: gp3
  encrypted: "true"
```

The StatefulSet's **PVC retention policy remains Retain** for both scale and
workload deletion. This prevents the StatefulSet controller from removing claims
before celld has supplied proof. The operator alone initiates PVC deletion.
The StorageClass/PV **Delete reclaim policy** then delegates the backing disk's
removal to CSI.

## Current-operation protocol

1. Capture the exact PVC name/UID/resource version, PV name/UID, CSI driver and
   volume handle. Require the configured class, dynamic provisioner annotation,
   `Delete` reclaim policy and
   `external-provisioner.volume.kubernetes.io/finalizer` on the PV. Missing
   protection blocks shutdown; the operator never changes storage policies.
2. Capture celld's exact `remove-disk` / `data_safe` result and independent launcher
   proof: the child exited, the inherited lock was reacquired, and disk-scoped
   restart denial is durable. A replacement Pod cannot restart celld on that
   retired disk. A missing or failed result blocks disposal indefinitely.
3. Apply and observe the exact guarded workload effect and target Pod removal.
   Recheck that effect on each cleanup attempt, including after controller restart.
4. Persist cleanup intent and the current PVC resource version in the same bounded
   operation. Recheck disk/claim identity and lack of any Pod reference, then issue
   a UID/resource-version guarded PVC deletion. A lost response is recovered by
   observing these recorded resources. A conflicting version requires revalidation
   and a new reservation CAS before retry; a replacement UID is never accepted.
5. Keep all current strict proof until the old PVC and PV are absent and no
   VolumeAttachment references the recorded PV name. A terminating object, stuck
   finalizer, changed reclaim policy/handle/UID or remaining attachment keeps the
   operation pending. The controller has read access to PVs and attachments and
   cannot remove their finalizers or force detach them.
6. Complete contraction or fleet deletion only after that observation. Restart
   and upgrade hold zero replicas through cleanup, then create fresh claims and
   resume. Proof is discarded only when the full operation completes. No retired
   disk ledger or positive-proof archive accumulates.

`DiskCleanupPending` is a projection of the current cleanup phase. It supplies
no independent deletion authority and is false after completion. A released
historical Retain PV, edited status, or an absent Pod is never sufficient proof.

## What completion establishes

For a conforming CSI provisioner, Kubernetes' deletion finalizer keeps a Delete
PV present until the backing storage has been deleted. Observing that exact PV's
absence after the protected cleanup therefore completes the CSI contract. See
[Kubernetes persistent-volume deletion protection](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#persistentvolume-deletion-protection-finalizer).
The controller does not call EC2 or directly delete backend volumes.

This relies on the CSI implementation and on administrators preserving its
finalizers and resource bindings. Manually removing protection, changing the PV
policy/source, or bypassing the operator is outside the contract. The operator
will block when it observes such drift; it cannot reconstruct an unobserved
administrative override from an already absent resource.

Unit regressions cover identity/policy/finalizer drift, lost responses, stale
issuers, attachment waits, restart/delete sequencing and fresh growth. Envtest
uses a real API server to exercise UID/resource-version preconditions and PVC/PV
finalizers; CSI completion is simulated there. The 100-member bounded-state test
includes every cleanup intent and captured protection flag. These checks do not
qualify EKS/EBS. Real CSI teardown, underlying EBS disappearance, and acknowledged
writes across shrink/grow and failure still require the cloud qualification run.
