# Bucket expired-record GC review

18 September 2026; base `57124bb` plus the PersistentFleet implementation and
review corrections in this change. Bucket expiry/settling behavior existed in
`583164f`; this is an existing lifecycle liveness gap, not a new launcher path.

Pinned celld `dead_node_gc.rs:357–411` retains folded peer-log tombstones, but
CAS-tombstones and then deletes expired non-folded Bucket records. The previous
operator required such a record to remain readable through its ten-second
settling period and only persisted GC-safe expiry authority at final completion.
The original failed live run lacks the intermediate reservation snapshots needed
to determine whether GC won before or after the first positive expiry read.

The corrected journal v6 records `ExpiryObserved` after a successful complete
assessment of an already-issued operation. It continues complete fresh survivor,
node and log/loss scans during settling. Missing records without that positive
observation remain blocked. Unissued or canceled admission cannot set the flag.
A typed same-generation lease-renewal or generation-replacement observation persists `ExpiryInvalidated`
in both in-flight candidates and fully settled history. Later disappearance
cannot reuse superseded proof; fresh positive expiry is required again.

The controller regression reloads journal/API state at each boundary and covers:
missing before expiry observation; Retired admission without expiry authority;
GC after durable positive expiry but before settling; live renewal resetting
settling; renewed then missing records for both in-flight and fully settled
history; replaced then missing records; fresh expiry restoring progress; and repeated contraction.

Final `make check` (including race tests and lint), `make manifests-check`,
`make qualification-replay` and `make qualification-test` pass. See adjacent logs.
A single bounded isolated Kind rerun is recorded separately; it is not AWS or
production automatic-capacity qualification.

The final replacement→missing negative-evidence correction was added after the
live manager had been built. It only adds invalidation on a positively observed
mismatched generation and renames the typed invalidation error; the exercised
normal lifecycle and same-generation renewal path are unchanged. Current-source
controller/adapter race tests and lint were rerun afterward and passed (see
`negative-evidence-race.log` and `lint.log`).

## Bounded live rerun outcome

The isolated `celld-step2-6cd2520d` run passed startup, isolation, strict placement,
unsafe-drift checks, manual Bucket 3→2→1, subsequent growth to three, the unchanged
PersistentFleet contraction block, pause fencing, and resume across a manager
restart. Automatic contraction completed 3→2. Its journal trace records positive
expiry before settling and completed retirement for the manual decrements; the
previous failing stage therefore passed with the correction.

The overall live suite remains **incomplete**. At 01:13:21–27 UTC on 19 September,
one surviving alpha runtime and all three beta runtimes self-fenced with exit
code 3 after ambiguous/failed node-lease renewals. Kubernetes restarted them.
The alpha replacement generation then prevented further automatic contraction;
capacity coverage was incomplete and historical session resolution was blocked.
The cause of the renewal failures was not established. This is separate from
the fixed loss of already-observed expiry evidence. No process restart,
generation replacement or missing record was accepted as recovery proof.

Previous-container logs, pod termination status, fleet status, reservation
snapshots and the compact authority trace accompany the full harness log.
The run was not retried to obtain a passing result. GC before any first positive
expiry observation also remains an explicit conservative liveness limitation.

The bounded run exited 1 after its 420-second automatic-contraction timeout.
It did not execute the later post-automatic harness assertions. Its owned Kind
cluster and local resources were removed, and Docker VM
`fs.inotify.max_user_instances` was restored to the original 128. No cloud or
default-kubeconfig access occurred. Unrelated Docker resources remained untouched.

The auxiliary authority sampler stopped on a ten-second Kubernetes API read
timeout after recording the first automatic decrement; it is a partial trace,
not a complete trace through the runtime failure. The later explicit reservation,
fleet and pod snapshots and previous-container logs cover the self-fence event.
