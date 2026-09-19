# Capabilities and limitations

celld operator is experimental software for evaluation. Every fleet must set
`qualification: Experimental`. The operator targets AWS EKS with S3 and EBS, but
cloud deployment, storage handoff, and failure testing have not been completed.

This is the current capability matrix. **Implemented** means the controller has
an execution path with the listed prerequisites. It does not mean that path has
passed production or AWS validation. Historical test reports describe only their
recorded versions and environments.

## Fleet operations

| Operation | Current behavior | Prerequisites and limits | Validation boundary |
| --- | --- | --- | --- |
| Create a fleet | Implemented for Bucket and PersistentFleet | Dedicated S3 bucket, runtime identity, namespace access, enforced NetworkPolicy, and eligible nodes. PersistentFleet also needs retained EBS storage. | Local Kubernetes testing; AWS validation pending. |
| Add replicas manually | Implemented in both profiles | Node capacity in the configured zones; new PersistentFleet disks must be created by the operator. | Local Kubernetes testing; real cloud capacity response pending. |
| Remove Bucket replicas manually | Implemented, one member at a time | Healthy remaining members and verified expiry of removed runtime sessions. Ordered layout permits deterministic removal; Deployment layout may block on strict placement. | Local runtime and Kubernetes testing; EKS/S3 failure validation pending. |
| Remove PersistentFleet replicas manually | Implemented with the launcher | Normally leaves at least two running members. A 2-to-1 change needs one configured zone and explicit whole-fleet downtime permission. | Local launcher and Kubernetes tests; real EBS handoff and failure validation pending. |
| Recommend capacity | Implemented in Shadow mode | Metrics Server and complete, fresh runtime observations. Recommendations do not change replicas. | Local tests; thresholds need workload-specific evaluation. |
| Add replicas automatically | Implemented in ScaleOut or Automatic mode | Explicit bounds, Metrics Server, stable demand, and useful new capacity. The operator does not provision cluster nodes. | Local tests; real capacity-pool response pending. |
| Remove replicas automatically | Unavailable against AWS evidence | Selecting Automatic does not bypass this limit. Executed only in the disposable local test fixture. | Production automatic contraction remains disabled. |
| Restart the same runtime version | Implemented | Placement must remain valid. PersistentFleet rolling restart needs two survivors; smaller fleets need explicit coordinated downtime. Applications must tolerate interrupted requests and connections. | Controller tests; positive real-runtime rolling restart/final shutdown validation remains incomplete. |
| Upgrade the runtime | PersistentFleet v0.4.1 to v0.5.0 only | Launcher, explicit whole-fleet downtime, stopped writers, sealed logs, and unchanged retained disks. No rollback or Bucket version transition. | Local released-process upgrade experiment passed; full Kubernetes upgrade orchestration and AWS testing pending. |
| Change Bucket layout | One-way Deployment to Ordered migration | v0.5.0, a migration token and explicit whole-fleet downtime. Storage and fleet identity stay unchanged. | Controller tests; live cluster migration validation pending. |
| Delete a fleet | Implemented RetainData shutdown and compute cleanup | Removal waits for verified shutdown. The operator completes the finalizer after cleanup; storage, reservation, recovery records, network isolation and credentials remain. | Controller tests; positive real-runtime final shutdown validation remains incomplete. |
| Recover an unreachable retirement target | Optional EC2 termination for a captured PersistentFleet target | Dedicated tagged node, scoped IAM permission, fencing settings and explicit operation annotation. This is not general node repair. | Fake EC2 tests; real AWS failure testing pending. |

Use the [scaling](../site/src/content/docs/operate/scaling.md),
[capacity policy](../site/src/content/docs/operate/capacity.md),
[restart](../site/src/content/docs/operate/restart.md),
[runtime upgrade](../site/src/content/docs/operate/upgrade-runtime.md), and
[deletion](../site/src/content/docs/operate/deletion.md) guides for commands and completion checks.

## Unsupported recovery and migration paths

- Restarting an all-stopped PersistentFleet when required logs did not seal. Keep the disks and recovery records; no supported automatic retry exists.
- General recovery from an uncertain node failure before the operator captured the exact runtime and host identity.
- Converting a PersistentFleet created without the launcher or with ReadWriteOnce claims to the launcher/ReadWriteOncePod layout.
- Adopting pre-existing persistent claims, transferring a bucket reservation to another fleet, or automatically freeing retained storage.
- Changing storage, placement, profile, or runtime ServiceAccount after fleet creation.
- Disruptively repairing a changed workload template. Drift is reported for investigation.
- Treating missing S3 metadata as proof that a writer stopped. Metadata removed before the operator observes expiry can leave an operation blocked.

The [blocked operations guide](../site/src/content/docs/troubleshoot/lifecycle.md)
explains what to collect and when an administrator must investigate.

## What to expect during maintenance

Connections and in-flight requests can be interrupted. Applications should reconnect
and handle retries; the operator does not replay requests or preserve in-memory state.
A healthy fleet may report `Ready=True` while a particular operation is blocked.
`LifecycleBlocked=True` and `ProductionQualified=False` remain set for this
experimental release; use `Blocked`, its message, and `status.lifecycle` to understand
the current action.

## Release and platform status

Use Kubernetes 1.31 or newer with IPv4 Pod networking and a CNI that enforces
NetworkPolicy. The [version reference](../site/src/content/docs/reference/compatibility.md)
lists accepted runtime images. The installation guide uses a locally built image
and repository chart; do not assume a development tag is published or pullable.

The provisional API group is `celld.example.com`. A stable release requires an owned
API domain, verified published artifacts, and completed cloud qualification.
[Contributor test reports](qualification/README.md) retain the detailed results and
open checks, including [the EKS test plan](qualification/eks-smoke-plan.md).
