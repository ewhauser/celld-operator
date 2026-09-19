# Leader loss during an issued contraction (kind)

19 September 2026. `make integration-leader` ran in the isolated three-node Kind
cluster `celld-step2-f6182578` with two manager replicas under controller-runtime leader
election. See [integration.log](integration.log).

## Scenario

With alpha at three replicas, the harness scaled the manager Deployment to two
replicas, recorded the elected leader from the `celld-operator.celld.eric.dev`
Lease, requested a contraction to two, and polled until the replica effect was on
the Deployment while the journal still held the open operation. It then deleted
the leader pod.

## Result

The standby acquired the Lease and completed the same operation. The Deployment's
spec generation advanced exactly once across the leader change, the journal holds
exactly one completion record for that operation ID, and the acknowledged
application write remained readable. The window was caught, not missed: the
operation was open when the leader was removed.

## Limits

This proves the journal's restart safety across a controlled leader loss with a
graceful Lease release. It does not exercise a hard leader kill with Lease
expiry (fifteen seconds by default), simultaneous loss of both replicas, or
leader loss during a PersistentFleet stop; those are separate scenarios. Not
AWS qualification.
