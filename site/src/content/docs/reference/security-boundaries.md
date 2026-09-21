---
title: Security boundaries
description: Separate runtime data authority, launcher process authority and Kubernetes effects.
---

The operator uses Kubernetes RBAC, private runtime control-plane access and an
authenticated launcher channel. It has no S3 SDK or EC2 termination authority.
celld alone holds the fleet's runtime bucket identity.

## Kubernetes access

The cluster-scoped role covers fleet discovery/status, permanent reservations,
current PV/StorageClass/node inspection and attachment observations. Each fleet
namespace has a separate Role for workloads, Pods, Services, policies, credentials
and guarded PVC creation/deletion. The controller's leader-election lease is
scoped to its own namespace.

Use the shipped chart and `config/rbac/fleet-namespace.yaml` as the exact permission
reference. The tests audit actual reconciler calls against these grants.
The operator does not remove CSI/PVC finalizers, force detach volumes, patch PV
reclaim policy or grant itself cloud credentials.

## Private listeners

| Port | Allowed callers |
| --- | --- |
| 8080 application | Same-namespace Pods labeled `celld.eric.dev/client-of: <fleet>`. |
| 8081 celld internal | Same-fleet peers and trusted operator-namespace Pods. |
| 8083 launcher | Trusted operator-namespace Pods; HMAC authenticates every request and response. No Service exposes it. |
| 8082 health / 8084 metrics | Operator probes and configured monitoring; restrict with your cluster policy. |

Generated NetworkPolicy requires an enforcing CNI. The operator's enforcement
flag is an administrator attestation, not a network implementation. Never expose
the unauthenticated celld internal listener through public ingress.

## Proof and trust

An immutable per-fleet Secret supplies the launcher HMAC key. Exact Pod,
container, host/boot, invocation, generation and disk identities bind the response
to the captured target. The runtime result is positive data-safety authority;
the launcher's lock and restart denial independently establish process exclusion.

The contract assumes administrators do not rewrite current-operation authority
or bypass storage protection, and native runtime processes retain their inherited
lock while they can access the disk. Local locks cannot fence a different kernel;
cross-host/boot reuse is refused.

All recovery participants must understand the fork's proof. Pin and qualify the
runtime and launcher artifacts. See [current operations](../../contracts/current-operation/),
[disposable disks](../../contracts/disposable-disks/) and
[qualification](../../qualification/) for boundaries and tests.
