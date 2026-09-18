# ADR 0001 Controller architecture

Status: Proposed

Date: 2026-09-18

## Context

The design proposal separates Kubernetes process capacity from celld's authority over cells, leases, replication, and handoff. This architecture is the planning baseline, but the discussion has not separately approved its language, framework, or API shape.

## Proposal

Build a Go operator using controller-runtime and Kubebuilder around a namespaced `CelldFleet` resource. One installation manages multiple independent fleets.

Separate capacity recommendations from lifecycle execution. The capacity policy chooses desired replicas; the lifecycle reconciler applies changes and serializes controlled removals. Record operations durably in Kubernetes before destructive steps and resume them after leader failure. Exactly one writer owns desired capacity at a time.

Celld continues to own membership, cell placement, replication, and recovery. The operator controls workloads, resources, Services, configuration, and lifecycle gates.

## Consequences

Manual and automatic reductions need the same restart-safe lifecycle foundation. That foundation belongs in the first scaling implementation, not a later hardening phase.

The CRD schema, API group, field ownership, operation phases, and external HPA integration remain implementation-plan decisions. The sample manifest in the original proposal is not an accepted API contract.
