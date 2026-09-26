# Typed runtime control-plane client

`internal/runtime/controlplane` owns celld HTTP transport and wire decoding. The
capacity collector uses its `/state` response and the load decoder in this
package. Metrics Server observations and the closing Pod-incarnation check remain
in place. `Lifecycle` is the shared interface for Bucket and PersistentFleet;
`runtimeTarget` derives their respective node names (Pod UID and Pod name) and
requires a current, owned Pod with an exact IP. No Service address is used.

Calls have a two-second deadline, a one-MiB response limit, duplicate-key and
nesting checks, no proxy discovery, and no redirects. The internal listener stays
on port 8081 under existing NetworkPolicy rules. This API has no authentication
credentials in the runtime contract. No Services, network policy, credentials or
RBAC are changed.

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
- Node-log state decodes `/state.node_log`; see [below](#node-log-state).

Ordinary shutdown/reload APIs cannot atomically bind a runtime generation. The
client rejects requests that ask those APIs to enforce one; it does not
simulate safety with a racy GET followed by an unguarded POST. There are no
controller callers of these mutations, and no fleet consumes the strict
`remove-disk` API since the [launcher](launcher-supervision.md) was retired.
PersistentFleet instead reads node-log state and disrupts
[one member at a time](current-operation.md); private S3 evidence, journal
archives and old release adapters have no active reconciliation path. An
explicitly verified compatible fork digest is still required for rollout.

PersistentFleet advertises each stable StatefulSet Pod DNS name through the
headless peer Service, which publishes addresses before readiness. celld consults
the predecessor lease address while recovering retained follower logs, before it
can publish a new lease. Advertising an ephemeral Pod IP makes those old peer
addresses unreachable after replacement and blocks recovery. Older `.2` builds
could declare bounded loss despite those retained disks. Bucket uses a fresh
runtime identity and Pod address.

The required `.3` fork keeps an unavailable recovery witness undecided regardless
of lease age. It serves retained follower data during the existing bounded
startup retries and refuses to seal the predecessor or declare loss merely
because a peer has not started. If retries are exhausted, startup fails with
recovery data retained. Stable addressing and this runtime behavior are both
required; `.2` lost acknowledged writes under ordinary startup skew.

The runtime retains its explicit-loss policy when reachable members
conclusively report missing or incomplete fragments and no complete witness
remains. Kubelet restarts an exited celld container on the same disk. The
operator replaces a member whose disk is gone and does not inspect private
recovery metadata. See the [delayed-witness evidence](qualification/native-peer-startup/README.md).

## Node-log state

`0.5.1-ewhauser.5` and later add `node_log` to `GET /state`: the node's
durability `posture`, `session`, its own log, `shipper_healthy` (a follower
ensemble is attached) and `fleet`, the last dead-leader sweep. The sweep carries
`observed_ms`, `complete`, `unrecovered` sessions with their state and lease
expiry, and `obligations`, a map from member node to the leader sessions whose
current epoch still needs that member's fragment. Required fields must be
present; a partial object is an error, not a healthy default. An absent or null
`node_log` reports `ErrNoNodeLog`, which the operator treats as unknown.

`internal/fleethealth` turns these reports into two answers: whether the fleet
has settled since the last disruption, and whether a removed member's disk is
still needed. See [one disruption at a time](current-operation.md#settlement).

## Strict schema alignment

No fleet consumes this API now. The client and fixtures remain for the
retained launcher package.

The adapter follows the implemented `State::snapshot` in
`crates/celld/disk_removal.rs` and `handle_internal` in `crates/celld/main.rs` on
`ewhauser/celld`, first published in `v0.5.1-ewhauser.2` (upstream v0.5.1 base).
Stock v0.5.1 does not implement this extension.

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
still succeeds while capacity marks that node unavailable. There is no
private-S3 fallback in this client.

celld drains existing HTTP connections before entering control-only mode. After
an accepted strict request, the launcher retries incomplete transport reads
within the original operation deadline while the exact child remains alive.
It never replays the mutation or treats a connection failure as proof. HTTP
rejections, malformed results, identity mismatches and runtime failures remain
terminal; completion still requires a fresh matching `data_safe` observation.

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
celld shutdown, S3 recovery or EBS removal. The opt-in real-binary tests described in [qualification](qualification/README.md)
exercise the launcher/controller handshake separately. See the native and Kind
records for replicated recovery; verify CSI/EBS deletion in your deployment.

## Application deployment observations

The typed client's separate read-only `ApplicationReader` uses the existing
`GET /state` response's `deployment` object: `version`, `prefix`, `generation`,
`cells` and `swapping`. It requires explicit counts and cell maps, bounds strings,
and rejects malformed data. A missing deployment object is unsupported. The
existing `shutdown.runtime_generation`, when present, supplies diagnostic process
identity; application observations do not require a new lifecycle schema.

Resident-cell generations are compared only against that node's current local
generation. Unknown cell generation zero remains pending; a cell generation newer
than the sampled node generation invalidates the observation. The actor census
and current generation are not sampled atomically. Response freshness is measured
locally by the operator and says nothing about the age of the S3 deployment pointer.

This works with the currently pinned runtime without another runtime patch or
image release. Existing strict-shutdown, image requirements and capacity contracts
are unchanged. The observer never calls `/reload` or reads S3 credentials.

See [application deployment observations](fleet-api.md#application-deployment-observations)
for aggregation and the limits of convergence. Tests cover malformed responses,
missing fields, stale observations, rollback, mixed versions, delayed cells and
membership changes; envtest verifies status admission. The opt-in
`hack/test-application-state.py` harness exercises native celld with disposable
MinIO, including deployment, a resident-cell transition, rollback and the limit
that a missing deployment pointer cannot be detected from serving-version state:

```sh
CELLD_TEST_BINARY=/absolute/path/to/celld \
CELLD_ESBUILD=/absolute/path/to/esbuild \
python3 hack/test-application-state.py
```

It needs Docker and the AWS CLI, uses only explicit loopback S3 endpoints and
fixture credentials, and cleans up its own runtime and container. Set
`CELLD_STATE_FIXTURE` to capture responses for decoder regression tests. The
checked-in fixture was captured from celld revision `c91ca54`, without the proposed
application-observation patch. This is native runtime coverage, not EKS validation.
