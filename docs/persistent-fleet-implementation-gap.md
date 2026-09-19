# PersistentFleet execution and retained-disk reuse

> Historical analysis (18 September 2026). It motivated the trusted launcher; the
> restricted same-host path it asked for is now implemented in
> [persistent-fleet-lifecycle.md](persistent-fleet-lifecycle.md), and the current
> feature matrix is [critical-features.md](critical-features.md). The analysis below
> remains the reason unwrapped invocations and uncertain node failures stay blocked.

At the time of writing, implementation was blocked: no production removal or
reactivation path was enabled in this investigation. Bucket behavior was unchanged.

## What the current metadata contract cannot establish

The production reader can inventory node generations, recovery epochs and loss
markers. Runtime recovery can fence an expired session's S3 lease chain and
recover its own peer log. That does not prove that the old process stopped or
lost access to its retained filesystem. The accepted PersistentFleet contract
requires exactly that additional authority before reusing an identity and disk.

A real pinned-image experiment demonstrates the distinction. Two isolated local
celld nodes were started. The first node was paused, preserving the invocation
and volume mount. After 12 seconds, a replacement with the same node name and
same named volume was started. Its node record acquired a different generation
and it became Ready in 1.077 seconds while the original process remained alive,
paused, and attached to that same filesystem. Thus successful predecessor
handling and replacement readiness do not imply exclusive filesystem access.

See [the captured result](qualification/persistent-reuse/results.json),
[original node record](qualification/persistent-reuse/before-node.json), and
[replacement node record](qualification/persistent-reuse/replacement-node.json).
The experiment did not resume simultaneous writers, inject peer-log appends, or
prove data loss. It did not exercise EBS or actual Kubernetes force deletion.
It is a counterexample to an exclusive-open guarantee in the runtime, not proof
that ordinary graceful StatefulSet scale-down corrupts data. No peer-only
acknowledgements were generated or qualified by this experiment.

Reproduce using the fixed local MinIO fixture and the unchanged image:

```sh
.qualification-venv/bin/python hack/qualification/persistent_reuse.py \
  --output .qualification-runs/persistent-reuse-new
```

The script uses a fresh network, MinIO instance, node containers and named
volumes, a 240-second deadline, explicit local credentials, and cleanup in
`finally`. It neither loads kubeconfig nor accesses AWS. Both executed runs
cleaned their owned resources.

## Why a seal cannot substitute for disk exclusivity

All runtime references below are pinned to commit
`12d5b6333fe52717325addcfe1e99e9fd4f77bcd`.

* [FollowerStore](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L1089-L1129)
  caches fragment state and serializes read/modify/write using process-local
  mutexes. A successor process does not share these locks or caches.
* [Follower seal](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L1767-L1817)
  persists a fragment seal under that process-local lock.
* [Follower routes](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L2724-L2827)
  authenticate the fleet request but do not establish that the receiving
  follower exclusively owns its local disk. An expired own lease is not an OS
  filesystem-access fence.
* [Recovery before install](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/main.rs#L4259-L4305)
  protects the predecessor's folded log record; it also serves retained
  follower files during startup. That is necessary recovery behavior, not a
  mechanism for terminating the predecessor invocation.

The controller creates ReadWriteOnce claims. Do not infer per-process exclusion
from their names, UIDs, ownership labels or mount configuration. Kubernetes also
explicitly documents that force-deleting a StatefulSet pod can create a second
process with the same identity; its absence is not termination evidence.
See [Kubernetes force deletion](https://kubernetes.io/docs/tasks/run-application/force-delete-stateful-set-pod/).

There is a separate follower-retirement obligation: the departing node's own
sealed log says nothing about fragments it holds for another live leader. The
runtime [acknowledges only after every selected follower confirms](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L3359-L3477),
but the operator has not qualified removal while a retained disk holds the only
peer copy of another node's acknowledged, not-yet-uploaded data. Do not convert
an empty client-ledger mismatch list from an ordinary workload into proof of
that case. Loss declarations must still be checked after any recovery seal.

## Concrete implementation needed

Choose and implement a trusted termination/exclusivity authority before enabling
the existing removal machinery. This is a missing component, not another
boolean precheck. Possible designs are:

1. **Infrastructure authority:** independently verify irreversible termination
   of the exact host/process incarnation and prevent its restart/storage access.
   An EC2 integration requires a separate permission and infrastructure-identity
   design; no such IAM expansion was made here.
2. **Trusted runtime launcher and storage contract:** bind the child invocation
   to a durable operation, hold a volume-exclusive lock throughout its entire
   lifetime, forward signals, enforce restart spacing, and prove exclusion
   across host changes under the supported storage driver's fencing semantics.
   Keeping the celld binary unchanged is possible in principle, but a launcher
   alone does not prove cross-host EBS fencing or automatically authenticate the
   runtime generation. This is a proposal, not a qualified implementation.
3. **Upstream supported runtime contract:** an exact generation identity and
   durable retirement/exclusive-reuse protocol covering follower files, with
   documented behavior for partitions and resumed old processes.

After establishing that authority, the remaining controller implementation is:

- Capture the exact highest-ordinal invocation and all unresolved sessions.
- Preserve the volume UID and verify exclusive retained-PVC ownership on reuse.
- Connect the authority to the durable one-decrement CAS, loss scan, survivor
  checks, recovery and settling already outlined by the shared machinery.
- Resolve historical generations only with positive recovery evidence; retain
  the same operation across lost responses and controller restarts.
- Add a durable reactivation phase that waits for exclusion, predecessor recovery
  and restart spacing before restoring the ordinal.
- Qualify repeated shrink/grow, stale issuers, crashes in every new phase, and
  actual peer-only acknowledgements under donor/survivor failure.

Automatic production contraction still requires EKS/S3/EBS qualification after
implementation. Administrator-written attestations, elapsed time, missing pods,
lease expiry, a new Ready generation, or a single sealed log are not substitutes
for the missing authority. This document does not change ADR 0015 or accept a
new failure contract on the user's behalf.
