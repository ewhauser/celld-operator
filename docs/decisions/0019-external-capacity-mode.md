# ADR 0019 External capacity mode and the /scale subresource

Status: Implemented

Date: 19 September 2026

## Context

The design proposal made capacity mode one of Manual, Automatic or External,
with External delegating the desired count to one HorizontalPodAutoscaler
through `/scale`. The operator shipped Shadow, ScaleOut and Automatic and no
`/scale` subresource; the pre-release review recorded the omission as an open
scope decision.

## Decision

Expose `/scale` on CelldFleet, mapping `spec.replicas`, `status.replicas` and
`status.labelSelector`. `status.replicas` counts the fleet's non-terminal pods
including terminating ones; the selector matches exactly this fleet's pods and
is what an HPA uses to read pod metrics. The subresource exists regardless of
mode: a write through it is a `spec.replicas` edit with the field's existing
semantics (a manual command that wins the next intent), so `kubectl scale`
behaves like editing the field.

Add `capacity.mode: External`. In this mode the built-in policy neither
computes a target nor collects samples, other policy fields are ignored, and
`status.capacity` reports `ExternalOwner` with desired and applied counts. The
operator never writes `spec.replicas`, so it cannot oscillate against the HPA;
a count it cannot apply stays visible as desired versus applied with the
blocking condition.

Contraction requested by the external writer is automatic in effect and shares
Automatic mode's production release gate (`ExternalContractionUnqualified`
outside the disposable fixture). Additions proceed through the journaled path.
Every other lifecycle gate, fence and evidence rule applies unchanged.

## Consequences

The HPA's `minReplicas` must be at least `placement.azCount`; the CRD refuses
lower writes and the HPA then reports a failed scale. An HPA attached to the
child workload instead of the fleet bypasses the journal and is reported as
replica drift (`LifecycleBlocked`), never adopted. Custom metrics adapters and
per-fleet scaling budgets remain external concerns. The local `--external`
kind suite exercises a real HPA and Metrics Server against `/scale`; it is not
production qualification of HPA-driven contraction.
