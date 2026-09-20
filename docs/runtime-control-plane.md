# Typed runtime control-plane client

`internal/runtime/controlplane` owns celld HTTP transport and wire decoding. The
capacity collector uses its `/state` response and the load decoder in this
package. The superseded v050 state decoder and v041 state-compatibility test and
fixture are removed. Metrics Server observations and the closing Pod-incarnation check remain
in place. `Lifecycle` is the shared interface for Bucket and PersistentFleet;
`runtimeTarget` derives their respective node names (Pod UID and Pod name) and
requires a current, owned Pod with an exact IP. No Service address is used.

Calls have a two-second deadline, a one-MiB response limit, duplicate-key and
nesting checks, no proxy discovery, and no redirects. The internal listener stays
on port 8081 under existing NetworkPolicy rules. This API has no authentication
credentials in the runtime contract; the authenticated launcher API remains a
separate boundary. No Services, network policy, credentials or RBAC are changed.

## Supported operations

- State discovery returns the runtime generation and explicit optional capability
  fields. Capacity requires schema 1 and a runtime generation; old or unknown
  schemas are rejected.
- Ordinary shutdown and preserve shutdown retain `/shutdown` and
  `/shutdown?handoff=preserve`. A successful response reports acceptance only.
- Reload retains `/reload` and reports the adopted/unchanged deployment generation.
  That numeric generation is distinct from the runtime ownership generation.
- Strict shutdown uses `/shutdown?mode=remove-disk`, with exactly
  `operation_id` and `expected_generation` in the JSON body. Preflight requires
  the versioned capability; the server enforces the generation on the mutation.

Ordinary shutdown/reload APIs cannot atomically bind a runtime generation. The
client rejects requests that ask those APIs to enforce one; it does not
simulate safety with a racy GET followed by an unguarded POST. There are currently
no controller callers of these mutations. The [launcher](launcher-supervision.md) uses the strict API to capture completion
before terminating celld. The controller now uses the
[bounded current-operation executor](current-operation.md); private S3 evidence,
journal archives and old release adapters have no active reconciliation path.
An explicitly qualified fork digest is still required for rollout.

## Strict schema alignment

The adapter follows the implemented `State::snapshot` in
`crates/celld/disk_removal.rs` and `handle_internal` in `crates/celld/main.rs` on
`ewhauser/celld`, branch `codex/strict-disk-removal` (upstream v0.5.1 base).
Stock v0.5.1 does not implement this extension. The old prototype's
`/lifecycle/retirement` and S3 archive are not used.

`GET /state` adds a `shutdown` object containing:

```json
{
  "schema_version": 1,
  "runtime_generation": "exact-generation",
  "capabilities": {"strict_disk_removal": true},
  "control_only": true,
  "operation": {
    "operation_id": "scale-in-42",
    "expected_generation": "exact-generation",
    "mode": "remove-disk",
    "phase": "data_safe",
    "blocker": null
  }
}
```

The operation can be null. Its phases are `draining`, `data_safe`, and `failed`;
`failed` includes deadline failures. There is no separate `result` field. The
runtime does not return a node name: exact routing and the expected runtime
generation bind the observation, while Kubernetes supplies node/Pod association.
Unknown schemas and absent/false capabilities never authorize strict shutdown.
There is no old-runtime capacity fallback.

Strict POST returns HTTP 202 with that shutdown object directly, also on a
same-operation retry. The client returns only `Acceptance` from POST, even if a
retry response includes `data_safe`. HTTP 200 from an older API is rejected.
HTTP 409 is a conflict, 400 rejects malformed/unsupported requests, and 501 means
an unsupported runtime configuration. Rejection codes remain available through
`HTTPError`.

`RemovalStatus` obtains a fresh `/state`, requires schema 1 and capability true,
checks both runtime and operation generations plus the exact operation ID, and
reports data safety only for `data_safe`, `control_only: true`, and a null
blocker. A lost operation, replacement process, malformed response, network
failure or deadline cannot become completion. The process remains alive in the
control-only phase. The current handler returns only `shutdown` after joining
the actor. Lifecycle decoding is independent of load fields: terminal polling
still succeeds while capacity marks that node unavailable. The launcher supplies exact termination and restart exclusion; the executor
must persist the current operation and result before authorizing deletion.
There is no private-S3 fallback in this client.

## Validation boundary

HTTP fixtures cover identity/operation mismatches, retries, acceptance versus
completion, failed/deadline/lost results, absent/unknown capabilities, malformed,
oversized and duplicate JSON, redirects, timeouts and cancellation. Collector
coverage runs for both profiles, preserving Metrics Server and Pod race checks.

`testdata/shutdown-v1.json` was generated by compiling the runtime's actual
`State`/`snapshot` implementation together with its actual `Control` source,
without building or running the full celld server. Regenerate it with:

```sh
python3 hack/qualification/controlplane-fixtures.py /path/to/celld-strict-shutdown
```

The fixture-generation source SHA256 values were:

- `crates/celld/disk_removal.rs`: `58d6ae8ea16b19ebb6ae283e8e7ac2d4d51107574d9e969e259a656dda7271ce`
- `crates/logic/disk_removal.rs`: `4ae763abcf9e2657880970ed0d0ea82d6e90341c11c13999cfade9c591df4b89`

This validates the implemented wire serializer against the Go client, not real
celld shutdown, S3 recovery or EBS removal. The sibling branch remains under
validation; a deployed runtime and the later lifecycle integration still need
independent qualification.
