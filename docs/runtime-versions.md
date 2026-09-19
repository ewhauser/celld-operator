# Released runtime transitions

The operator recognizes exactly the canonical multi-platform release index digests:

| Release | Source commit | Image SHA256 |
| --- | --- | --- |
| v0.4.1 | `10cb1303dac710dcb3b557e318e08c855261f68b` | `ce8bbc3c26a16c9ee00e3ce0501f36bfea2663b5af8285a08fc16a54568060a5` |
| v0.5.0 | `12d5b6333fe52717325addcfe1e99e9fd4f77bcd` | `df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8` |

Both are `ghcr.io/denoland/celld@sha256:<digest>`. An omitted runtimeImage selects v0.5.0. Mutable tags, arbitrary digests and architecture aliases are not deployment identities. v0.4.1 creation requires PersistentFleet with the trusted launcher.

The [upstream v0.5.0 release](https://github.com/denoland/celld/releases/tag/v0.5.0) explicitly requires stopping the whole v0.4.1 fleet before upgrading. This is the sole supported directional transition. Same peer protocol 5 does not authorize a rolling upgrade. No documented reverse storage-compatibility guarantee exists; v0.5.0 to v0.4.1 rollback is rejected and retained as an unsupported request.

Set `spec.maintenance.allowCoordinatedDowntime: true` and change `spec.runtimeImage` from the v0.4.1 digest to v0.5.0. The controller journals exact source, target and original replica count; captures every exact invocation; obtains durable launcher stop/restart-deny receipts; requires every captured own log sealed and expired; then scales to zero and waits for all pods to disappear. Only at that boundary does a StatefulSet resource-version CAS install the target container configuration and an operation-specific annotation. A lost response is recovered from that exact annotation, image and zero-replica state. It resumes the original count and requires new generations, retained PVC/PV/disk identity and stable evidence before promoting the journal's applied image.

v0.4.1 requires an explicit drain-token timeout under the configured shutdown budget; v0.5.0 rejects that removed setting. The image CAS also changes this version-specific environment atomically. Other workload settings remain checked against the exact generated template.

The registry uses the shared protocol-5 node/load/log evidence codec, justified against the pinned release sources: [v0.4.1 peer auth](https://github.com/denoland/celld/blob/10cb1303dac710dcb3b557e318e08c855261f68b/crates/celld/peer_auth.rs), [ownership store](https://github.com/denoland/celld/blob/10cb1303dac710dcb3b557e318e08c855261f68b/crates/celld/ownership_store.rs), and [node log](https://github.com/denoland/celld/blob/10cb1303dac710dcb3b557e318e08c855261f68b/crates/celld/node_log.rs). Regression fixtures include actual private `/state` and node metadata captured from the unchanged v0.4.1 release on local Docker/MinIO.

Controller tests execute the durable source-to-target sequence, preserve claims, require replacement generations, reject rollback/tampered authority, and recover a lost image-update response while rejecting a stale issuer. This is not EKS qualification. The local `hack/qualification/versions.py` experiment separately exercises actual released processes and retained Docker volumes; its result must be checked before claiming live cross-version compatibility. It does not validate the Kubernetes executor. An all-stopped fleet with any unsealed own log remains blocked; retained disks alone do not permit bypassing the source recovery barrier. No Bucket version transition or rollback is implemented.

The [local released-image experiment](qualification/runtime-upgrade/README.md) now passes: 12/12 source acknowledgments survive the forward transition; six additional target writes bring the cross-node verification to 18/18. See its explicit quiescence procedure and limitations; this does not close the live Kubernetes operator upgrade qualification gap.
