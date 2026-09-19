# Ordered Bucket local qualification

The disposable Kind run completed successfully on 2026-09-18 America/Denver
(2026-09-19 UTC). Evidence is in [kind.log](kind.log).

Command: `python3 hack/integration/run.py --ordered-bucket`.

The tested controller was built from the agent worktree based on `add5c2c`, plus
the Ordered Bucket layout, exact-generation supersession, and the
`initialClaims` PersistentFleet-only guard. It predates the parent's subsequent
steady-state Bucket admission change; that change has separate unit validation.
This run does not establish a pass for every feature in the final combined tree.

## Observed results

- Three real Bucket pods started through the operator's scheduling gates on
  three distinct Kind nodes labeled across two simulated AZs.
- Every ordinal matched its assigned AZ, required hostname separation held,
  and the workload used `emptyDir` without creating PVCs.
- Manual contraction from three to two removed only the highest ordinal,
  preserving the exact UIDs of both survivors.
- Twelve successful application writes remained readable after contraction.
- Graceful process termination restarted an unchanged celld container inside
  the same Pod UID, producing a new ready runtime invocation.
- Growth back to three created a new highest-ordinal Pod UID.
- After an operator restart, a second contraction from three to two completed.
- The durable journal contained an exact `SupersededBy` generation relationship,
  and all twelve acknowledged writes remained readable again.
- An application client could not access the private `/state` endpoint.

The earlier run exposed a real provisioning bug: `initialClaims` assumed every
StatefulSet had a PVC template. The fix restricts claim allocation to
PersistentFleet. A full Reconcile provision-and-expand regression test now
covers Ordered Bucket fleets without PVCs, in addition to the live rerun.

Focused Go race tests passed for controller, runtime adapter, and API packages.
The live scenario also exercises real Metrics Server collection and Calico
NetworkPolicy enforcement; it does not replace adversarial protocol tests.

## Environment and limits

The run used Kind Kubernetes v1.31.4, Calico v3.29.3, Metrics Server v0.8.0,
MinIO, and pinned celld image
`ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8`.

The unique test cluster `celld-step2-485ac429` was removed by the harness.
The Docker VM's inotify instance limit was temporarily raised from 128 to 1024
for local cluster bootstrap and restored to 128 afterward. A prior harness cache
attempt exposed invalid CRI metadata for digest-only Docker image archives;
digest-only images now use normal registry pulls. Tagged cached images are
exported for the explicit node platform.

No AWS resource or default Kubernetes context was used. Simulated node/AZ labels
are not real EKS/AZ failure qualification. This scenario does not cover automatic
contraction, node partitions, arbitrary process overlap, metadata GC before first
positive evidence, upgrade/deletion execution, or steady-state admission before
the first removal. Physical termination is not inferred from logical membership
or supersession observations.
