# Strict control-plane local qualification

This is a historical `.2` run. Use the [`.3` qualification](../native-peer-startup/README.md)
and [actual `.2 → .3` upgrade](../runtime-upgrade/README.md) for the current
artifact. A later hosted `.2` run exposed startup-skew data loss despite stable
DNS; `.2` is not the recommended runtime.


Date: 2026-09-20. This directory records actual local Kind results and their
failures. It does not establish AWS/EBS or production qualification.

## Latest published-runtime run

**PASS, exit 0:** `make integration` at
`6ea528756e721d189eea5ab7283a1e3ab45ba815`, owned three-node cluster
`celld-strict-b936d780`. Operator and launcher were built from that exact source.
The native Linux arm64 runtime was the published fork `0.5.1-ewhauser.2`, source
`2a65a4df99bed5254bdd679df57ed98556454dec`:

- Index: `ghcr.io/ewhauser/celld@sha256:a00da2bcaeaee6879d658477cd1bdb354a5de55fa9e7f0ab5e2fd95e6e0ce080`.
- Executed arm64 manifest: `sha256:e4014d56abd9b8830a0a4a908fd8cdcd806a8b2220892bc4d3d962b9f9319e06`.

The run uses real Kubernetes workload controllers, an in-cluster operator and
launcher, Kubernetes v1.31.4, Calico v3.29.3, Metrics Server v0.8.0, MinIO and
Toxiproxy. Persistent storage uses the upstream distributed hostpath CSI driver,
external-provisioner v6.3.0, ReadWriteOncePod and Delete/WFFC storage. MinIO uses a
bounded 2 GiB memory-backed emptyDir. No unrelated resources were pruned.

The [compact event log](integration-ewhauser2-stable-dns-events.log) records every
PASS/FAIL marker. [result.json](result.json) records exact identities, counts and
SHA256 receipts for the complete run and all four pre-replacement runtime logs
archived outside Git. The owned cluster was removed and the observer exited zero.

Recorded results:

- Both profiles passed growth and contraction through 2 to 1, pause-before-issue,
  controller replacement, automatic contraction, stable surviving Pod UIDs and
  bounded idle state. PersistentFleet observed exact PVC/PV deletion and fresh
  PVC/PV/CSI identities on regrowth.
- HPA and `kubectl scale` passed through the fleet `/scale` endpoint, including
  strict contraction, observed status and return to manual ownership.
- Both profiles passed actual manager crashes immediately before and after the
  guarded replica update, exact-target strict proof validation, resumption of
  the same operation, and exactly one workload replica update. The injected S3
  latency case also passed.
- All four runtimes self-fenced after S3 lease expiry, with authenticated launcher
  `ExitedUnrequested` observations and no strict removal authority. Restoring S3
  did not restart them or alter Pod/PVC/PV identities. Explicit administrative
  replacement used UID preconditions and normal deletion grace with no current
  lifecycle operation. PersistentFleet retained its exact disk and host/boot
  identity; both profiles recovered all **36/36** acknowledged IDs, including the
  fresh batch written immediately before partition.
- Both profiles completed coordinated restart across manager replacement. All
  **48/48** acknowledged IDs per fleet remained readable. PersistentFleet used
  fresh PVC/PV/CSI identities after observed deletion of retired storage. A
  second controller restart did not replay the token. Final deletion removed all
  compute and PersistentFleet PVCs/PVs while retaining both bucket reservations.

Each phase adds twelve unique IDs across twelve distinct cells, retains every
prior ID and requires the returned ID and stored value to match. No false stored
value is retried into a pass. The stronger oracle is not retroactive evidence for
the older runs below.

The supplemental [read-only endpoint observer](ewhauser2-stable-dns-pod-rebinding.json)
confirmed actual DNS rebinding during recovery:

| Pod | Old IP | New IP | Unchanged advertised endpoint |
| --- | --- | --- | --- |
| beta-0 | 192.168.20.70 | 192.168.20.77 | beta-0.beta-peers.fleets.svc:8081 |
| beta-1 | 192.168.17.10 | 192.168.17.12 | beta-1.beta-peers.fleets.svc:8081 |

Both Pod UIDs changed and both nodes stayed the same. The maintained harness now
asserts changed IPs and expanded DNS directly; this older executed revision used
the independent observer for those checks.

This local run predates launcher transport-observation retry `ed09768`. A later
hosted run at this source exposed transient `/state` EOF during shutdown; the
retry has separate deterministic, Linux and native handshake checks; final
hosted results are recorded separately.
Do not attribute that later code to this local execution. The separate local
validation manifest is
`/Users/ewhauser/.codex/artifacts/celld/0.5.1-ewhauser.2/operator-final-local-validation.json`.

## Earlier attempts and defects found

1. [Initial environment failure](integration-initial-storage-pressure.log):
   MinIO returned `507 Insufficient Storage` before a fleet started. The isolated
   fixture switched from container overlay to bounded memory storage. The Docker
   VM also needed the documented inotify instance limit of 1,024 for three
   kubelets; its watch limit was already 1,048,576.
2. [Old all-suite failure](integration.log), compiled at `92c433e` against `.1`:
   lifecycle, HPA, crash boundaries and latency passed, but the old assumption
   that S3 restoration automatically restored readiness was false after lease
   expiry. The child had self-fenced, and the next read failed to connect.
   Maintenance was not reached. A [recorded fixture intervention](local-fixture-intervention.txt)
   removed only a completed manager-labeled network probe; `a2d5cc6` fixed the
   ambiguous crash-log selection permanently.
3. [Revised fault run](faults-lease-loss-recovery.log), `make integration-faults`
   at `1bd7545` against `.1`, passed four crash cases and explicit same-host
   administrative recovery. Its twelve historical IDs were in one cell and did
   not include an immediate fresh-tail batch, so it does not establish the
   stronger durability result.
4. [Hosted fresh-tail failure](https://github.com/ewhauser/celld-operator/actions/runs/35541999300/job/106161255348)
   at `4490a20` against `.1` was a real acknowledged-write loss: the first fresh
   beta value was missing after retained-disk recovery, while the earlier twelve
   beta values and all twenty-four alpha values were readable. A matched `.2`
   native reproduction isolated stale Pod-IP peer addresses; stable endpoints
   recovered all ten values per node, whereas changed endpoints recovered none.
   The controller now advertises stable PersistentFleet headless DNS. See the
   [native peer-address evidence](../native-peer-addresses/README.md).
5. The first `.2` local attempt at `516120c`, cluster
   `celld-strict-22d06b2a`, was intentionally interrupted after setup when the
   independently reproduced peer-address defect was fixed. It has no fresh-tail
   result. Only its owned cluster was cleaned and its read-only observer exited.
   The complete canceled log is archived outside Git, with its hash in
   [result.json](result.json).

Earlier `.1` runs used source `f3b7e8c07e6fee53f1752bfb7a30fffbf1d514c8`, index
`ghcr.io/ewhauser/celld@sha256:78f74de9b5482a428b69f175cd1901b59cc363f3aa398ffd197ade0c9a6a20af`,
and arm64 manifest `sha256:5e0ac98b4ef6d626db46af2c28228524e5d2aa34011d00d70979f32179d6c433`.

## Limits

Recovery above is an explicit administrative same-host replacement of failed,
nonretired Pods, not automatic restart or generic cross-host disk reuse. No
retirement marker or host-binding file was reset. That `.2` runtime's bounded-loss
policy when predecessor peers are unavailable or sufficiently delayed at startup
was a separate limitation of this run; stable DNS alone did not fix it. The
linked `.3` report records the runtime correction.

These fixtures do not replace the MinIO Pod and do not establish object-store
durability across its replacement. They do not qualify EBS deletion, EC2 loss,
prolonged S3 partitions or managed services. A genuine different-runtime upgrade
requires a second compatible immutable fork digest via `CELLD_UPGRADE_IMAGE` and
was not supplied. Unknown strict completion deliberately blocks removal.
