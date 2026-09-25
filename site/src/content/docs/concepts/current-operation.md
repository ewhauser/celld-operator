---
title: How operations resume
description: One bounded current operation survives controller restarts and guards each Kubernetes effect.
---

This applies to PersistentFleet. Bucket fleets have no current operation: the
workload controller rolls one member at a time, and the reservation keeps only
applied count, image, restart token and capacity-policy state.

For PersistentFleet, the storage reservation holds one current operation, its fixed deadline and
exact target identities. It also holds current workload and claim bindings,
bounded capacity-policy state and one last completion projection. It keeps no
append-only session or completion archive.

1. **Intent:** record the target and desired request. Pause or a changed request
   can cancel only while issuance has not been authorized.
2. **Requesting:** a reservation resource-version comparison authorizes the
   operation. Reconcile retries the same operation and generation without
   extending its deadline.
3. **Proof captured:** persist celld's strict completion and the launcher's
   independent process/exclusion proofs.
4. **Apply and observe:** compare the workload UID, resource version and
   predecessor effect marker before changing replicas. The exact marker
   reconstructs a successful write whose response was lost.
5. **Storage cleanup:** delete only the captured claim with UID/version
   preconditions and wait for the captured CSI volume and attachments to clear.
6. **Resume or complete:** maintenance starts on fresh disks. Complete after
   observing the intended workload result, then discard the operation's proof.

After Requesting, pause, a new token, reversal or deadline expiry cannot erase
an uncertain issued request. Missing completion remains blocked. New requests
wait for the current one to finish.

A successor claim with the same name has a different UID and cannot be deleted
by an old cleanup request. Status, Pod absence and HTTP acceptance cannot replace
captured proof. Do not edit reservation annotations to clear a blocker.

See [the implementation contract](../../contracts/current-operation/) and
[disposable disks](../../contracts/disposable-disks/) for exact state and cleanup
rules. The encoded current authority has a hard 180 KiB limit.
