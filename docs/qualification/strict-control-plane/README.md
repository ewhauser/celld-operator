# Strict control-plane local qualification

Date: 2026-09-20. This directory records local Kind evidence, including failures.
It is not AWS/EBS qualification or a production release certificate.

The command was `make integration`, using the harness compiled at
`92c433e`. This included the bounded-operation controller, permanent disk-scoped
launcher exclusion, CSI Delete cleanup contract and Parallel Ordered Bucket
management. The run uses actual Kubernetes workload controllers, an in-cluster
operator and launcher, and native Linux arm64 celld on a three-node Kind cluster.
Kind uses Kubernetes v1.31.4, Calico v3.29.3, Metrics Server v0.8.0, MinIO and
Toxiproxy. Persistent storage uses the upstream distributed hostpath CSI driver
with external-provisioner v6.3.0, ReadWriteOncePod and Delete/WFFC storage.

The runtime input is the fork release `0.5.1-ewhauser.1`, built from
`f3b7e8c07e6fee53f1752bfb7a30fffbf1d514c8`:

- Index: `ghcr.io/ewhauser/celld@sha256:78f74de9b5482a428b69f175cd1901b59cc363f3aa398ffd197ade0c9a6a20af`.
- Executed arm64 manifest: `sha256:5e0ac98b4ef6d626db46af2c28228524e5d2aa34011d00d70979f32179d6c433`.
- The fork's multi-platform publication was verified separately; this local run does not execute the amd64 variant.

## Recorded runs

[The initial attempt](integration-initial-storage-pressure.log) failed before any
fleet started. MinIO returned `507 Insufficient Storage` on its container-overlay
volume. The harness now uses a bounded 2 GiB memory-backed emptyDir for this
disposable fixture. No unrelated images, volumes or clusters were pruned. The
Docker VM's inotify instance limit also needed the documented increase to 1,024
for three kubelets; the watch limit was already 1,048,576.

[The subsequent all-suite log](integration.log) records the workload assertions.
The [fixture intervention](local-fixture-intervention.txt) removed a completed
network probe that shared the manager's label and could confuse crash-log
selection. It did not change a fleet workload, operation or disk. Commit
`a2d5cc6` makes that cleanup automatic and reads crash evidence from the exact
manager Pod and container.

That all-suite run **failed** after a ten-second S3 outage and did not reach
coordinated maintenance. Both observed runtime logs show
`node_lease_watchdog_fence`: the outage exceeded their ten-second node lease TTL.
An unsolicited child exit is not strict removal authority under the launcher
contract. The harness's former assumption that restoring S3 would automatically
restore service was incorrect; Kubernetes readiness briefly lagged the process
exit, and the acknowledged-read attempt then failed to connect. The owned cluster
was removed normally after diagnostics.

Before that failure, both profiles passed repeated growth and contraction,
including 2 to 1; highest-ordinal removal with stable surviving Pod UIDs;
controller replacement; pause-before-issue; automatic contraction through Metrics
Server; and acknowledgement reads. PersistentFleet also passed observed PVC/PV
deletion and fresh PVC/PV/CSI identities on regrowth. External HPA and `kubectl
scale` passed through the fleet's `/scale` endpoint. Both profiles passed actual
manager crashes immediately before and after the guarded workload update,
resuming the same operation with exactly one replica write. Bucket contraction
and acknowledgement reads passed with injected S3 latency.

[The revised fault run](faults-lease-loss-recovery.log), `make integration-faults`
compiled at `1bd7545`, **passed** against the same `.1` runtime digest. It completed
all four actual manager crash/recovery cases and the latency scenario, then
observed every target through its authenticated launcher API. Each runtime
self-fenced after S3 lease expiry, reported `ExitedUnrequested` and child exit,
and supplied no strict removal proof. Restored S3 did not automatically restart
them; all Pod/PVC/PV identities and replica counts remained unchanged with no
current lifecycle operation.

The harness then performed explicit administrative replacement of only those
failed Pods, using UID preconditions and ordinary deletion grace. Recovery
produced new Pod, invocation and runtime-generation identities. PersistentFleet
retained its exact PVC/PV/CSI handle, Node name and UID, boot and launcher disk
identity. All twelve previously acknowledged values per fleet were read back.
No retirement marker or host-binding file was reset. The command exited zero and
removed only its own cluster and launcher image. This establishes the tested
same-host recovery path, not automatic recovery or generic cross-host disk reuse.

## Data oracle and limits

The recorded `.1` runs check twelve distinct acknowledged IDs in one cell per
fleet across the lifecycle transitions and repeat their reads through later suites.
The subsequent harness change `bb1aa47` strengthens this by creating twelve new
IDs in every phase and retaining all prior IDs in the read oracle. That later
oracle must be validated by its own run; it is not retroactive evidence for this
log. Later changes also parallelize the three registry pulls, wait for `/scale`
status projection and capture logs from every runtime ordinal.

Changes `003c44f` and `c988856` add a fresh batch immediately before the storage
partition and spread writes across twelve distinct cells. The `1bd7545` fault
run above does not establish that stronger acknowledged-tail result.

These fixtures do not replace MinIO's Pod and do not establish object-store
durability across its replacement. They do not qualify EBS deletion, EC2 node
loss, prolonged S3 partitions, managed-service behavior or a different-runtime
upgrade. A real upgrade requires `CELLD_UPGRADE_IMAGE` naming a second compatible
immutable fork digest. Unknown strict completion deliberately blocks removal.
