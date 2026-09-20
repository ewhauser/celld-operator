# Strict launcher supervision

The launcher owns one celld child and one current removal operation. celld owns
replication, tiering and recovery safety. The launcher consumes the typed client
in `internal/runtime/controlplane`; it neither reads S3 nor interprets load,
leases, cell counts or replication metadata.

This is item #3 of the lifecycle simplification. It requires celld's schema 1
strict disk-removal contract. Stock v0.5.1 cannot satisfy it. A homogeneous fork
release, including recovery readers that understand `bucket_complete`, and a
verified image digest remain deployment prerequisites.

## Handshake

1. The launcher acquires the local exclusive volume lock, seeds a fresh celld
   generation with `CELLD_REEXEC_PROBE_SIGNING_KEY`, and starts exactly one child.
   Its authenticated discovery response contains the Pod UID, node, host, boot,
   disk nonce, invocation, generation and PID. The fresh cryptographic generation
   binds requests to that invocation; a successor always gets a new generation.
2. The controller records the intended operation and expected Kubernetes/launcher
   identities before requesting removal. It POSTs an authenticated request to
   the private launcher listener on port 8083 at `/v2`:

   ```json
   {
     "Nonce": "64-hex-character-random-challenge",
     "Operation": "scale-in-42",
     "Generation": "exact-runtime-generation",
     "NotAfterMS": 1790000003000,
     "DeadlineMS": 1790000600000
   }
   ```

   These timestamps illustrate absolute Unix milliseconds. `NotAfterMS` is a
   short request replay bound (controller: at most three seconds; launcher:
   at most ten seconds to tolerate clock skew). `DeadlineMS` is the first
   accepted operation's deadline, at most 24 hours ahead. The controller passes
   its operation deadline through the context, separately from the HTTP timeout.
   Duplicate requests return the same operation without restarting it or
   extending its deadline. Conflicting operations, stale generations and new
   operations after Running are refused. Empty Operation means observation;
   observation remains available after the operation deadline.
3. The launcher calls `RemoveDisk` against local celld. The typed client checks
   capability and generation, then POSTs `/shutdown?mode=remove-disk` with the
   operation ID and expected generation. HTTP 202 is only acceptance, including
   a repeated POST whose body already says `data_safe`.
4. The launcher polls `RemovalStatus`. Only the typed client's validated schema,
   capability, runtime generation, operation ID, operation generation, mode,
   `data_safe`, `control_only: true` and null blocker supply data safety. Final
   `/state` may contain only `shutdown`; actor/load decoding is never required.
5. The launcher captures that result in memory **before** sending SIGTERM to its
   exact process handle. celld's control-only loop accepts this signal as final
   termination. The launcher never sends a conflicting ordinary `/shutdown`.
   If the child does not exit within its termination grace, SIGKILL ends it;
   neither signal nor exit code supplies data safety.
6. After `Wait` confirms exact child exit, the launcher closes its inherited
   descriptor without `LOCK_UN` and reacquires the same lock independently. It
   checks both file identity and the original random lock token. Surviving
   descendants keep the inherited lock and prevent completion or a second writer.
7. While holding the reacquired lock, the launcher durably creates/fsyncs the
   Pod UID's restart-deny marker. Only then does it publish `Stopped`. It keeps
   the lock and serves the current result until the Pod is terminated.

The response carries `Removal` (operation, generation, mode, phase, blocker,
control-only and validated data-safe flag), `ChildExited`,
`InheritedLockReleased` and `RestartDenied` separately. `RemovalReady()` requires
all of them plus `Stopped`. The controller verifies response nonce and HMAC,
Pod/node/host identity, nonempty invocation/generation and the requested
operation/generation. Its existing callers additionally compare the captured
invocation and disk identity. A signed `Stopped` response missing any proof is
rejected.

There is no old endpoint or old wire-format compatibility, launcher handoff
request, positive receipt archive, automatic child restart or second execution
engine. Requests/responses retain their domain-separated HMAC authentication;
existing private networking and credential binding remain in force.

## Failures and exclusion

Capability absence, request rejection, status failure, timeout, a mismatched or
lost operation, malformed response and HTTP outage all withhold successful
removal authority. A failed or ambiguous capture is terminal for this launcher
operation. It preserves celld's recovery service until the fixed deadline, child
exit or Pod termination; termination after that point cannot repair the missing
proof. Duplicate requests cannot retry a failed capture into success.

A requested stop can end with `ChildExited`, `InheritedLockReleased` and
`RestartDenied` true while its operation remains `Failed` and data safety is
false. An unsolicited child exit reports `ExitedUnrequested`, holds the local
lock and refuses late operation adoption. Linux retains parent-death signaling,
PID-1 orphan reaping, and the inherited descriptor across fork/exec. Pod absence
and expired leases cannot substitute for these proofs.

A launcher crash loses its in-memory result. If Kubernetes has not durably
captured completion, removal remains blocked. The negative disk marker only
refuses another launch of that Pod UID; it never reconstructs data safety,
termination or a positive receipt. Same-host successors must first acquire the
inherited lock. A different host or boot is blocked before child startup because
local flock cannot establish exclusion across kernels. Preserve-mode generation
overrides (`.clean-reload.json`) are refused.

## Operator integration

[Bounded current operations](current-operation.md) now capture this result in
Kubernetes before any compute or PVC removal. Completed operations discard
runtime proof. Retained PV/EBS deletion and cross-host disk policy remain separate
qualification work; neither negative restart markers nor the operator's
DiskCleanupPending condition provide a positive historical deletion receipt.

## Verification

`make check` runs build, race tests and native/Linux lint. `make test-linux`
executes the process/lock/crash tests in a Linux container. Its stock runtime
image supplies the test environment and shell fixtures; it is not a compatible
strict runtime qualification.

The opt-in test starts isolated MinIO, deploys a minimal worker, then runs a real
strict celld child through the supervisor and HTTP client:

```sh
CELLD_STRICT_TEST_BINARY=/absolute/path/to/strict/celld \
CELLD_STRICT_TEST_ESBUILD=/absolute/path/to/esbuild \
go test -race ./internal/launcher -run '^TestStrictRuntimeSupervisorHTTP$' -v -count=1
```

The September 20 local macOS ARM64 run used binary SHA256
`840fac6de89d3083db9945aa28e099bd9776cc9786ac7ca2b11fe9ec5bdcdd9a`, built from
`celld-strict-shutdown` (verified via its dependency file and the runtime task's
qualification artifact). It captured the control-only result, exact seeded
generation, child exit, inherited-lock release and restart denial. This empty-disk
handshake does not qualify replicated recovery, a published runtime image, or
EKS/EBS removal. Those remain separate integration gates.
