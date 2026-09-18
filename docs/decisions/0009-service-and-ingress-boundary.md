# ADR 0009 Service and ingress boundary

Status: Accepted

Date: 2026-09-18

## Context

The operator needs to expose application traffic while fitting into the user's existing network and certificate management.

## Decision

Expose each fleet's application endpoint through a ClusterIP Service. Users manage public ingress or gateways, TLS, and public DNS outside the operator.

Keep celld's internal peer/operator listener separate from public application routing. Peers need individually reachable pod addresses; a load-balanced Service address must not stand in for a node identity. A headless Service for persistent pod discovery is compatible with this boundary.

## Consequences

The initial operator does not provision public load balancers, certificates, DNS records, or ingress controllers. Existing infrastructure must route to the application's Service.

The runtime's internal APIs are powerful and require network isolation. Enforced NetworkPolicy, operator access, same-fleet peer traffic, and the EKS network prerequisites belong in the implementation and qualification plan.

Do not bypass celld readiness to make emergency scale-out appear successful. Useful capacity under pressure remains a release gate in [ADR 0010](0010-production-qualification.md).
