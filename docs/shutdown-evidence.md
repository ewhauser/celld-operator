# Shutdown evidence for repeated peer-disk contraction

> Historical record (18 September 2026). It describes the state at that step; the current feature matrix is [critical-features.md](critical-features.md) and the current journal version and compatibility rules are in [journal-archives.md](journal-archives.md).

Follow-up: [the launcher-managed graceful PersistentFleet path](persistent-fleet-lifecycle.md) implements a restricted same-host contract. The earlier analysis below remains relevant to unwrapped invocations and uncertain failures.

Current implementation: [shared lifecycle safety](shared-lifecycle-safety.md). Production collection is now wired; exact identity/fencing and mode-specific removal qualification remain blocked.
Implementation follow-up: [step 1 adapter, real local experiments, and remaining release gates](qualification/README.md). The earlier source conclusions below are historical; the follow-up distinguishes observations from unqualified hypotheses.

Reviewed 18 September 2026 against unmodified celld v0.5.0, commit `12d5b6333fe52717325addcfe1e99e9fd4f77bcd`. This follows [runtime qualification](runtime-qualification.md). This assessment traces released source; it does not claim an end-to-end or AWS test.

## Conclusion

Follow-up: [read-only S3 recovery evidence](s3-recovery-evidence.md) defines a proposed adapter after the user accepted metadata access. It corrects an important limitation of interpreting a seal: celld can seal after recording possible loss, so positive sealing evidence must also be checked against loss declarations.

The existing HTTP status and Kubernetes container exit status do not provide a reliable authorization for repeated PersistentFleet removals. Celld attempts the right durability work internally, but success and incomplete shutdown can produce the same externally observed exit code. Longer grace periods improve the chance of completion; they do not turn termination into proof.

This is an observability limitation, not evidence that celld loses acknowledged data on every shutdown. Preserve all PVCs and stop further controlled removals when evidence is uncertain. Do not describe that behavior as fully qualified, repeatable peer-disk autoscaling.

## Evidence

All links below are pinned to the reviewed commit.

| Candidate evidence | What the implementation establishes | Sufficient for another removal? |
| --- | --- | --- |
| `POST /shutdown` returns `200 {"ok":true}` | The handler attempts to enqueue a shutdown mode and immediately replies; it even discards the channel-send result. [Handler](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L2912-L2928). | No. This is not a completion acknowledgement. |
| Container exit code 0 | The shutdown path reports stalled handoff or durability timeouts but still reaches `exit_flushed(0)`. [Failure reporting](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L5332-L5380), [quiesce](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L5420-L5435), [exit](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L5500-L5530). | No. A successful process exit is not a successful durability seal. |
| Quiesce completes before its deadline | `quiesce_and_seal_within` reports completion of a future returning `()`. Its `close_gracefully` call is best effort: unreadable state, incomplete coverage, or a failed seal write can return normally without sealing. [Wrapper](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L3741-L3774), [close](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L4970-L5061). | No; additionally this boolean is not an HTTP status field. |
| Zero drain counters or `restoring=0` | Core drain completion checks activation/ownership transition counts. Node-log sealing happens later. These counts do not inventory follower fragments held for other nodes. [Drain test](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L5173-L5191), [snapshot](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/actor.rs#L4245-L4275). | No. |
| Remaining pods are Ready | Fleet readiness is a one-shot gate, not a continuously refreshed recovery certificate. [Gate](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L4367-L4404). | No. |
| Wait for lease expiry plus a recovery interval | Survivors eagerly attempt recovery of dead sessions, including idle work, but reads and recovery can fail. This is not a fixed upper bound on successful completion. [Sweep](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L5731-L5801). | No. Elapsed time is not evidence of success. |

The runtime drain token also cannot replace operator lifecycle sequencing: it is explicitly advisory, and a donor may bypass acquisition after its wait budget. See [drain token semantics](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/drain_token.rs#L3-L12).

## The stronger signal exists only in logs

`node-log close: sealed epoch ...` is emitted after a successful seal write. The seal checks per-cell coverage and uncovered retained bundles. This is meaningful positive evidence for that node's own log, stronger than exit status. The recovery path similarly emits `node log recovered and sealed` with the dead session after successfully publishing recovery completion. See [close confirmation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L5025-L5059) and [recovery confirmation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L4875-L4920).

A digest-pinned log adapter or launcher is therefore a possible research direction without modifying celld, but is not qualified here. It would need to durably capture positive evidence, bind it to the exact pod/container incarnation and runtime session, distinguish trusted runtime output from application output, survive leader failures and log loss, and account for unresolved earlier sessions whose fragments might be on a later victim. Sealing the departing node's own log does not certify every other node's data on its disk. Absence of an error message must never count as success.

The `clean_reload_prepared` marker is for preserve-mode reloads, not normal SIGTERM handoff; it is not a fleet-wide follower-retention certificate. The peer `/peer/log/seal` route is a recovery-protocol mutation, not a read-only operator completion endpoint. Calling it would cross into the runtime's recovery authority rather than solve the status contract.

## Implementation implication

For the HTTP-only, no-operator-S3 design, persist an unresolved recovery condition after any peer-disk removal lacking authoritative completion evidence. Retain storage, continue monitoring, and allow compatible additive capacity. Do not clear the condition from exit code, pod disappearance, elapsed settle time, or healthy survivors alone. Apply the same rule to upgrades and planned restarts, and reassess after uncontrolled node losses.

Supporting routine peer-disk contraction still requires qualifying an additional evidence channel: a supported runtime API in a future release, a carefully verified adapter for existing runtime logs, or an explicitly accepted read-only storage-metadata contract. None is established merely by increasing termination grace. Bucket-profile qualification can proceed separately.
