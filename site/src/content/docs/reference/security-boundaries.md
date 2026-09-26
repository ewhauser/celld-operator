---
title: Security boundaries
description: Separate runtime data authority from Kubernetes effects.
---

The operator uses Kubernetes RBAC and private runtime control-plane access. It
has no S3 SDK or EC2 termination authority.
celld alone holds the fleet's runtime bucket identity.

## Kubernetes access

The cluster-scoped role covers fleet discovery/status, permanent reservations,
current PV/StorageClass/node inspection and attachment observations. Each fleet
namespace has a separate Role for workloads, Pods, Services, policies, credentials
and guarded PVC creation/deletion. Service updates only fill unset fields a newer
release declares on operator-created Services. PodDisruptionBudget updates
converge fleet budgets. The controller's leader-election
lease is scoped to its own namespace.

Use the shipped chart and `config/rbac/fleet-namespace.yaml` as the exact permission
reference. The tests audit actual reconciler calls against these grants.
The operator does not remove CSI/PVC finalizers, force detach volumes, patch PV
reclaim policy or grant itself cloud credentials.

## Private listeners

| Port | Allowed callers |
| --- | --- |
| 8080 application | Same-namespace Pods labeled `celld.eric.dev/client-of: <fleet>`, plus explicitly selected routing data-plane Pods when enabled. |
| 8081 celld internal | Same-fleet peers and trusted operator-namespace Pods. |
| 8082 health / 8084 metrics | Operator probes and configured monitoring; restrict with your cluster policy. |

Generated NetworkPolicy requires an enforcing CNI. The operator's enforcement
flag is an administrator attestation, not a network implementation. Never expose
the unauthenticated celld internal listener through public ingress.

## Runtime state and trust

No fleet uses a launcher, key Secret or proof channel. celld owns durability:
Bucket writes are in S3 before acknowledgement, and PersistentFleet writes are on
every follower's disk. The operator reads each member's unauthenticated
`/state.node_log` over port 8081 to decide when the fleet has settled and when a
removed member's disk may be deleted. Anyone who can forge that response inside
the fleet's network boundary could delay changes or release a disk early, which
is why the internal listener must stay private.

The contract assumes administrators do not bypass storage protection. The
`celld.eric.dev/replace-member` annotation deletes a member's existing disk, so
treat CelldFleet edit access as authority over fleet data.

All recovery participants must understand the fork's proof. Pin and qualify the
runtime artifact. See [one disruption at a time](../../contracts/current-operation/),
[retained disks](../../contracts/disposable-disks/) and
[qualification](../../qualification/) for boundaries and tests.

## Admission mutations

Mutating admission such as service-mesh sidecars, workload-identity credentials,
telemetry injection, registry mirrors and policy-engine hardening may add
containers, init containers, environment, volumes and mounts. No injector is
named or allow-listed, and the operator no longer validates admitted Pod shape.
Mutations that change the runtime image, command, operator-set environment or
data-disk mounts can break celld; keep them out of fleet namespaces. Admission
that can add privileged or hostPath containers is trusted with the node.

## Optional routing

Fleet editors can configure public hostnames and controller-specific Ingress
annotations. Treat this as ingress-administration authority and apply your
platform's hostname, annotation and certificate admission policies. The generated
routing NetworkPolicy selects both the data-plane namespace and Pod labels and
admits only TCP 8080. Labels identify selected workloads, not untrusted tenants.
The platform must verify the actual packet source; host-networked proxies and
external load balancers may require separately administered network rules.
Gateway listeners, TLS, DNS and ingress controllers remain platform-owned.

## Shared preview pools

Platform administrators configure `CelldFleet.spec.previews` and own child fleet
configuration and storage reservations. Developers create previews referencing a
permitted parent fleet.
Each preview has a separate runtime, prefix and route. Shared bucket credentials
do not enforce an IAM boundary between previews: pools are for one trust domain.
Use separate pools, buckets and identities for mutually untrusted tenants. Custom
store credentials are same-namespace Secret references injected by kubelet, not
read into controller status. See [application previews](../../contracts/previews/)
for disposable-store failure and retention behavior.

Seed executors write only the reservation status subresource; they cannot change
immutable requests or lifecycle annotations. This permission is cluster-scoped
because reservations are cluster-scoped. Scope fixed grants by resource name where
possible. See [preview seeding](../../contracts/preview-seeding/) for the protocol.
