# Strict launcher supervision

The launcher owns one celld child and one current removal operation. celld owns
replication, tiering and recovery safety. The launcher consumes the typed client
in `internal/runtime/controlplane`; it neither reads S3 nor interprets load,
leases, cell counts or replication metadata.

It requires celld's schema 1 strict disk-removal contract. Stock v0.5.1 cannot
satisfy it. A homogeneous fork
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
6. After `Wait` confirms exact child exit, the launcher durably creates/fsyncs
   the disk's permanent restart-deny marker while still holding the inherited
   descriptor. This blocks every Pod identity from reopening the disk, including
   during the subsequent lock reacquisition. The marker is negative authority
   only; it cannot reconstruct data safety or a successful stop.
7. The launcher closes its inherited descriptor without `LOCK_UN` and
   reacquires the same lock independently. It
   checks both file identity and the original random lock token. Surviving
   descendants keep the inherited lock and prevent completion or a second writer.
   Only then does it publish `Stopped`. It keeps the reacquired lock and serves
   the current result until the Pod is terminated.

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

Capability absence, request rejection, terminal status failure, timeout, a
mismatched or lost operation, and malformed responses withhold successful removal
authority. After one accepted shutdown mutation, incomplete status reads caused
by transport errors are retried within the original deadline while the exact
child remains alive. This covers celld closing keepalive connections before its
terminal control-only listener is ready. HTTP rejection, malformed JSON and
identity mismatch remain terminal; the mutation is not replayed by this poller.
A failed or ambiguous capture is terminal for this launcher operation. It preserves celld's recovery service until the fixed deadline, child
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
refuses every launch on that disk, including a replacement Pod UID; it never reconstructs data safety,
termination or a positive receipt. Same-host successors must first acquire the
inherited lock. A different host or boot is blocked before child startup because
local flock cannot establish exclusion across kernels. Preserve-mode generation
overrides (`.clean-reload.json`) are refused.

## Operator integration

[Bounded current operations](current-operation.md) now capture this result in
Kubernetes before any compute or PVC removal. The [disposable disk
policy](disposable-disks.md) retains that proof through CSI deletion, then
discards it at operation completion. Neither negative restart markers nor
status conditions provide a positive historical deletion receipt. Cross-host
reuse of an old disk remains blocked.

## Verification

`make check` runs build, race tests and native/Linux lint. `make test-linux`
executes the process/lock/crash tests in a Linux container. The image supplies
the test environment and shell fixtures; this alone does not qualify the
runtime's strict shutdown or recovery behavior.

The opt-in test starts isolated MinIO, deploys a minimal worker, then runs a real
strict celld child through the supervisor and HTTP client:

```sh
CELLD_STRICT_TEST_BINARY=/absolute/path/to/strict/celld \
CELLD_STRICT_TEST_ESBUILD=/absolute/path/to/esbuild \
go test -race ./internal/launcher -run '^TestStrictRuntimeSupervisorHTTP$' -v -count=1
```

The September 20 local macOS ARM64 run used binary SHA256
`f9b68e9e9608d74c40a9d8e9dab1846c3532f4f51762dc25e863c0986f2146db`, from the
verified `0.5.1-ewhauser.2` fork release. It captured the control-only result, exact seeded
generation, child exit, inherited-lock release and restart denial. This empty-disk
handshake does not exercise replicated recovery or CSI disk removal. Those
behaviors have separate integration records and deployment checks.

The [September 21 `.3` qualification](qualification/native-peer-startup/README.md)
repeats both real-binary handshakes and adds populated native recovery, hosted
Kind faults and a [real version upgrade](qualification/runtime-upgrade/README.md).
The earlier `.2` handshake is historical evidence, not a runtime recommendation.
