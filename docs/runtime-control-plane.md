# Typed runtime control-plane client

`internal/runtime/controlplane` owns celld HTTP transport and wire decoding. The
operator only reads `GET /state`: the capacity collector uses the runtime
identity it reports and the load decoder in this package, and the application
observer uses its `deployment` object. Metrics Server observations and the
closing Pod-incarnation check remain in place. `runtimeTarget` addresses both
profiles the same way and requires a current, owned Pod with an exact IP. No
Service address is used.

Calls have a two-second deadline, a one-MiB response limit, duplicate-key and
nesting checks, no proxy discovery, and no redirects. The internal listener stays
on port 8081 under existing NetworkPolicy rules. This API has no authentication
credentials in the runtime contract. No Services, network policy, credentials or
RBAC are changed.

## Runtime identity

The fork adds a `shutdown` object to `/state`, first published in
`v0.5.1-ewhauser.2` (upstream v0.5.1 base). Stock v0.5.1 does not implement it.
Capacity reads two of its fields:

```json
{
  "schema_version": 1,
  "runtime_generation": "exact-generation"
}
```

Capacity requires schema 1 and a runtime generation. An absent object, an
unknown schema or a missing generation yields no capacity sample; there is no
old-runtime capacity fallback. The version is read first, so an unknown future
shape neither breaks load collection nor supplies an identity. The object's
other fields, `capabilities`, `control_only` and `operation`, describe celld's
strict `remove-disk` shutdown, which no fleet has used since the
[launcher](launcher-supervision.md) was retired.

The operator makes no control-plane mutation: it never calls `/shutdown`,
`/reload` or strict `remove-disk`. PersistentFleet restarts one member at a time
through its StatefulSet ([PersistentFleet lifecycle](current-operation.md)). The
client does not decode the `node_log` object that `0.5.1-ewhauser.5` and later
add to `/state`. Private S3 evidence, journal archives and old release adapters
have no active reconciliation path. An explicitly verified compatible fork
digest is still required for rollout.

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
remains. See the [delayed-witness evidence](qualification/native-peer-startup/README.md).
Kubelet restarts an exited celld container on the same disk. The operator does
not inspect private recovery metadata. It replaces a member that cannot come
back, judged only from what Kubernetes reports about the member's Pod and
claim; it makes no control-plane call for this. See
[self-healing](current-operation.md#self-healing).

## Validation boundary

HTTP fixtures cover malformed, oversized and duplicate JSON, redirects,
timeouts and cancellation, and the identity requirements above, including a
captured `/state` response. Collector coverage runs for both profiles,
preserving Metrics Server and Pod race checks. This validates the wire decoder,
not real celld shutdown, S3 recovery or EBS removal.

The strict shutdown client, its fixture generated from the runtime's serializer
and the opt-in real-binary tests of the launcher/controller handshake were
removed once nothing called them; see Git history and the
[earlier evidence](qualification/README.md#earlier-evidence). See the native and
Kind records for replicated recovery; verify CSI/EBS deletion in your deployment.

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
image release. Image requirements and capacity contracts are unchanged. The
observer never calls `/reload` or reads S3 credentials.

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
