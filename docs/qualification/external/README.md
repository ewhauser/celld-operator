# External capacity mode with a real HorizontalPodAutoscaler (kind)

19 September 2026. `make integration-external` (`hack/integration/external.py`)
ran in the isolated three-node Kind cluster `celld-step2-aa21913a` with Calico, MinIO,
Metrics Server and the in-cluster manager. All 30 stages passed; see
[integration.log](integration.log).

## What was exercised

- `capacity.mode: External` reports `ExternalOwner` and the `/scale` view
  shows `spec.replicas`, the observed pod count and the fleet selector.
- An `autoscaling/v2` HPA whose `scaleTargetRef` is the CelldFleet read the
  fleet through `/scale` and raised `spec.replicas` from 2 to its maximum of 3
  on real Metrics Server CPU data; the operator applied the addition through the
  journaled path. The fleet generation stayed constant for twenty seconds
  afterwards: neither writer rewrote the field.
- Lowering the HPA ceiling to 2 made it request contraction; the disposable
  fixture's gated Bucket executor completed 3→2 and the acknowledged application
  write remained readable. The `/scale` view agreed with the workload.
- With the HPA deleted, `kubectl scale celldfleet/alpha --replicas=3` behaved as
  an ordinary `/scale` writer, and dropping the policy returned `spec.replicas`
  to manual ownership.

## Limits

Three nodes with strict hostname separation bound the fleet at three replicas.
HPA-requested contraction executes here only because the fixture is
`--local-test`; in production it reports `ExternalContractionUnqualified`
until the Automatic release gate closes. This is not qualification of HPA
behavior on EKS, of custom metrics adapters, or of contraction under load.
