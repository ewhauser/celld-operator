# ADR 0004 Availability zone placement

Status: Accepted

Date: 2026-09-18

## Context

Users need control over how many availability zones a fleet uses and whether capacity can take precedence over that spread.

## Decision

Expose a per-fleet AZ count and placement strictness. Strict placement is the default: leave pods Pending when the requested placement cannot be satisfied. Allow an explicit per-fleet configuration that relaxes the requirement.

Use topology spread constraints and node/pod affinity rules to implement the chosen placement contract. Taints and tolerations are relevant to dedicated-node placement; they do not themselves distribute replicas across AZs.

## Consequences

Define eligible-zone selection, behavior for more available zones than requested, replica floors, and the exact relaxation policy when finalizing the API. Validate incompatible combinations instead of silently reducing isolation.

An existing EBS volume remains tied to its AZ. Relaxing pod placement must not replace a retained disk or reuse an identity with an empty disk.

Pod distribution is not a promise of cross-AZ durability for peer acknowledgements. The inspected runtime does not select followers using Kubernetes AZ labels. AZ-loss guarantees require separate qualification and cannot be inferred from pod spread. See [runtime qualification](../runtime-qualification.md).
