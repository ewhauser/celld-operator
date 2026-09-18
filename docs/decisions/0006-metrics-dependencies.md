# ADR 0006 Metrics dependencies

Status: Accepted

Date: 2026-09-18

## Context

Automatic scaling needs runtime load signals and container resource measurements, but deploying Prometheus must not be a prerequisite.

## Decision

Keep Prometheus optional. Use celld's runtime API and Kubernetes Metrics Server as the initial collection approach for automatic scaling. Expose operator metrics for users who choose Prometheus or another monitoring system.

Missing, stale, incomplete, or unrecognized observations are unknown, never zero demand. Contraction requires fresh evidence from all required nodes. Missing metrics must not prevent ordinary infrastructure reconciliation and status reporting.

## Consequences

Prometheus being optional does not make required scaling measurements optional. Define sample windows, receipt timestamps, coverage, restart behavior, and stabilization history in the implementation plan.

Use bounded collection and low-cardinality metrics. Application latency signals and any extra exporter remain qualification/design details; do not introduce a mandatory Prometheus dependency through a lifecycle gate.

The S3 reader in [ADR 0008](0008-recovery-evidence-and-conservative-removal.md) supplies durability evidence, not a replacement for load sampling.
