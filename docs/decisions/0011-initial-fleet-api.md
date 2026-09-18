# ADR 0011 Experimental initial fleet API and provisioning gate

Status: Implemented for step 2; production qualification remains blocked

Date: 2026-09-18

## API identity and placement

Use the namespaced `celld.example.com/v1alpha1` CelldFleet API. The repository
contains no evidence of ownership of a suitable API domain. `example.com` is an
explicit provisional domain, not an assertion of ownership. Establish an owned
domain before any stable release; this experimental API has no migration promise.
This adopts the Go/controller-runtime architecture proposed in ADR 0001.

Require `qualification: Experimental`. Default replicas to three, disk size to
10 GiB, and placement to Strict. These are prototype defaults, not capacity
recommendations. Pin the runtime to the step 1 image; expose no arbitrary image,
environment, pod template, scale subresource, or runtime argument escape hatch.

Require an explicit standard AWS AZ name list and an equal `azCount` (1–6), with
replicas at least that count. All zones belong to the explicit storage region.
Required node affinity limits scheduling to that list and Linux. Strict topology
spread uses maxSkew 1, minDomains equal to the count, DoNotSchedule, and honors node
affinity and taints. Missing zones leave excess pods Pending; this is not gang
scheduling and does not prevent partial startup. Relaxed uses ScheduleAnyway and
retains the same zone allowlist. Neither mode reselects zones or moves retained
volumes. Distinct follower AZs remain unqualified; Kubernetes placement does not
control celld follower selection. Dedicated node pools/tolerations and resource
sizing knobs can be added through a future lifecycle-aware API revision.

## Storage authority

Reserve the **entire canonical bucket**, not just a user-supplied prefix. The
initial runtime configuration does not expose prefix multiplexing. This is more
conservative than prefix reservations and prevents overlapping runtime metadata,
peer keys, or region/endpoint aliases from forming apparently independent fleets.
Alternate endpoints and bucket aliases are not part of the public API.

A cluster-scoped CelldStorageReservation is named from SHA-256 of the bucket.
Atomic Kubernetes Create arbitrates competing fleets, including across namespaces
and concurrent controller instances. Its immutable spec binds bucket, namespace,
name, fleet UID and the defaulted spec hash. No owner reference or automatic
release exists. Deleting/recreating a same-named fleet does not reclaim a reservation.
The operator is not authorized to delete reservations, workloads or PVCs.
Cross-cluster writers and external runtime processes require external governance
and IAM; one Kubernetes cluster cannot reserve storage against another cluster.

## Initial provisioning without a lifecycle bypass

The entire fleet spec is immutable through CRD CEL admission. Controller validation
and reservation spec hashes independently fail closed if admission is bypassed.
Step 2 also blocks additive changes: no scaling policy is implemented. Supporting
compatible additive changes remains planned; this initial API doesn't promise it.

Create NetworkPolicy, zero-voluntary-disruption PDB, ClusterIP application Service,
and headless peer Service before creating the workload. Deployments use Recreate;
StatefulSets use OnDelete and Parallel startup (avoids readiness join deadlocks).
Neither strategy is an authorization to update a template: the operator never
updates a workload. Its RBAC has only get/create on workloads. Existing mismatched
objects are reported and never adopted or repaired by mutation. Compare complete
controlled specs after normalizing explicitly known Kubernetes defaults and
allocated Service addresses. Additional probes, environment entries, containers,
volumes and selector constraints are drift, not default values. Finalizer patches
use optimistic resource-version locking to preserve concurrent finalizers.

Before initial PersistentFleet provisioning, reject any existing deterministic
ordinal PVC name, even if its labels match. Labels alone cannot establish disk
identity. After journaling intent, atomically create each PVC with the fleet UID
and reservation annotation, then create the StatefulSet. An AlreadyExists race
or a partial allocation blocks workload creation and retains every claim; the
operator never adopts an uncertain existing disk.

Persist a creation-attempt annotation on the reservation **before** PVC/workload Create.
Resource-version conflict serializes concurrent attempts. A crash after recording
intent but before creation is deliberately a manual-review blocker. A missing
workload after an attempt is not recreated automatically. The operator has only
get/create privileges on PVCs; it cannot update or delete them. This avoids treating
retained or unknown disks/identities as fresh state. Existing resources have a
fleet UID label and no owner references, so foreground deletion of the CR cannot
cascade while its lifecycle finalizer is blocking deletion. Reservations and PVCs
remain retained even after administrative removal of the CR.

The CR finalizer never completes automatically in this step. Administrative
namespace deletion, direct workload/Pod deletion, forced finalizer removal,
changes by another controller, node failures and kubelet restarts are outside
this control boundary. Restrict those permissions separately. PDBs constrain
voluntary eviction, not failures or direct deletion. The future lifecycle
controller must implement a documented teardown and recovery procedure.

## Runtime and network boundaries

Set durability explicitly to bucket/fleet. Use a disk-backed emptyDir for Bucket;
use one RWO claim per StatefulSet ordinal for PersistentFleet, with Retain on both
scale and deletion. Require an existing EBS CSI StorageClass with Retain reclaim
and WaitForFirstConsumer. The stable StatefulSet Pod name is its runtime identity;
Bucket uses Pod UID. A direct Pod IP is advertised for peers, never a load-balanced
Service address. Runtime membership is discovered through its bucket metadata;
the headless Service also exposes private pod endpoints including unready peers.

Use the unchanged image's shell to wait a full configured 10-second TTL before
**every container invocation**, then exec celld as PID 1. SIGTERM during the wait
exits without starting celld. After exec, signals go directly to celld. This is a
prototype spacing mechanism, not node fencing or proof of safe disk replacement.
The runtime image, shell, TTL and command are pinned together. Keep its public
health readiness gate; no liveness/startup probe restarts a pressured runtime.
A 20-second shutdown budget fits within 30-second Pod grace, without claiming
that time or exit status proves completed recovery.

Require an administrator to attest tested CNI policy enforcement using
`--network-policy-enforced`; otherwise no workloads are provisioned. NetworkPolicy
allows private port 8081 only from same-fleet pods in the same namespace or
explicitly labeled operator pods in the configured operator namespace. Public
port 8080 accepts explicitly labeled clients in the same namespace. Egress permits
same-fleet peers, kube-system DNS, HTTPS for S3/STS, and the EKS Pod Identity IPv4
endpoint. IAM must independently isolate storage; HTTPS egress is not an S3-only
firewall. Application arbitrary egress is intentionally unavailable in this API.

NetworkPolicies are additive. Namespace owners able to add allow-all policies,
spoof trusted labels, or create host-network workloads can bypass this boundary.
Use trusted namespace administration, restricted RBAC/admission and a policy-capable
CNI; this is not a hostile-tenant isolation claim. The operator cannot detect a
CNI that silently ignores policy. A local test exercises actual connection denial.

References: [Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/),
[StatefulSet retention and update behavior](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/),
[topology spread semantics](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/).
