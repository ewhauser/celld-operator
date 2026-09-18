# ADR 0005 Application disruption contract

Status: Accepted

Date: 2026-09-18

## Context

Scaling, upgrades, and runtime restarts can interrupt active requests and connections. The user accepted Cloudflare-style disruption behavior rather than requiring uninterrupted execution.

## Decision

Attempt graceful handoff, but permit interrupted HTTP/RPC requests, canceled in-flight work, and WebSocket disconnections. Applications must support reconnects and safe retries, including deduplication or idempotency where needed.

Preserve acknowledged durable writes within the supported and tested failure model. Do not promise preservation of in-memory state or arbitrary in-flight computation. The operator must not automatically replay ambiguous non-idempotent requests.

## Consequences

This is an application-facing expectation, not a claim of exact Cloudflare implementation parity or adoption of a Cloudflare SLA.

Shutdown budgets, recovery-time objectives, and acceptable interruption durations still require measurement. Longer termination grace improves the opportunity to hand off; it is not proof of successful durability completion.

Reference: [Cloudflare Durable Object lifecycle](https://developers.cloudflare.com/durable-objects/concepts/durable-object-lifecycle/), discussed and accepted on 2026-09-18.
