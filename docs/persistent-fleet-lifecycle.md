# PersistentFleet graceful lifecycle

18 September 2026. Experimental implementation around unchanged celld v0.5.0,
commit `12d5b6333fe52717325addcfe1e99e9fd4f77bcd` and the existing image digest.
This supplies an executable graceful retirement/reactivation path. It does not
qualify EKS/EBS, arbitrary node failure, or the last follower. Graceful cross-node
reuse within one AZ is implemented under the explicit CSI handoff contract below.

## Installing the launcher

The operator Dockerfile now builds `/celld-launcher` alongside `/celld-operator`.
Configure `--launcher-image=REGISTRY/OPERATOR@sha256:DIGEST` to opt new
PersistentFleet workloads into this version of the protocol. Production requires
a digest reference. A nonprivileged init container copies the binary into an
emptyDir; the celld container mounts that directory read-only and runs the
launcher. The original digest-pinned celld executable is not patched or rebuilt.
Bucket retains its existing launch command and volumes.

The operator exclusively creates an immutable per-fleet Secret containing a
random 32-byte authentication key, records its digest on the storage reservation,
and fails on uncertain adoption or replacement. The Kubernetes permissions this path
adds are Secret get/create, PV/Node/StorageClass get, VolumeAttachment list, and
Pod update (for scheduling gates). Secret and Pod verbs are granted only inside
fleet namespaces through the namespaced Role in
`config/rbac/fleet-namespace.yaml`; the ClusterRole holds no Secret, Pod or
workload verbs at all. There is no Secret list/watch/delete or EC2 permission
from this path. Runtime service accounts still do not receive Kubernetes API tokens.

Port 8083 is private: NetworkPolicy admits only operator pods in the configured
operator namespace, and no public Service exposes it. Every request and response
has a domain-separated HMAC; responses bind a fresh random challenge to pod UID,
node name, host, invocation nonce, generation and operation. Requests to stop
also bind the exact generation and expire after at most three seconds and before
the operation deadline. Retrying an accepted stop is idempotent; another operation
or a successor generation cannot inherit it. Redirects, oversized responses,
wrong identities and bad MACs fail closed. Only the first celld container
invocation within a pod is admitted (restart count zero); unexpected container
restarts require investigation. Controlled reactivation creates a fresh pod UID,
so an old launcher response cannot inherit a new container association.

## Lock lifetime and positive termination evidence

Before celld starts, the launcher acquires an exclusive flock on
`/work/.celld-launcher.lock`. It durably records the restrictive host/boot identity
and waits a full ten-second lease interval. It generates an Ed25519 seed and
passes it through celld's existing `CELLD_REEXEC_PROBE_SIGNING_KEY` handoff. The
pinned runtime consumes/removes that environment variable at boot and uses its
public key as the node lease generation. The launcher knows the expected public
generation independently of HTTP `/state` or address matching.

Sources: [probe signer installation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/peer_probe.rs#L70-L95),
[generation derivation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/ownership_store.rs#L562-L564),
and [actor lease generation](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/actor.rs#L2347-L2365).
The testing generation override is removed from the child's environment; a
preserve-mode `.clean-reload.json` marker blocks startup rather than changing
the asserted generation.

The child inherits the same locked open-file description as FD 3. The launcher
never calls `LOCK_UN`, which would release the shared lock while a child still
holds the descriptor. Linux parent-death SIGKILL is defense in depth; lock
inheritance supplies exclusion when a child or descendant outlives its parent.
Forked/execed descendants may conservatively hold the lock longer. Source review
found no descriptor-closing sweep in the pinned celld crates; Linux subprocess
and real-runtime tests separately exercise this assumption. This is a
version-specific integration, not a generic promise about arbitrary executables.

For a signed stop request, the launcher signals its exact `os.Process` child handle, waits
for that child, and escalates that same handle after 25 seconds if necessary.
It does not signal a numeric PID or process group after reaping; descendants
that remain alive keep their inherited lock and prevent completion.
It closes only its own lock descriptor and attempts an independent exclusive
open, reporting `ReleasingInheritedLock` while a descendant still holds the
inherited descriptor. For a requested stop that wait is unbounded (the pod
exists until the controller decrements); after an unrequested termination it is
capped at twenty seconds because no certificate is owed. Only successful
reacquisition of the same file allows `Stopped`; exit code 0 alone is
irrelevant. The re-open never creates the file and compares a token written
into the lock at first acquisition, because inode numbers are recycled on common
Linux filesystems: an unlinked or replaced lock file blocks instead of
certifying. The live launcher continues holding the new lock and serving the
exact stopped response until Kubernetes removes the pod. Before publishing that response it creates and
fsyncs a permanent deny marker for the retired Pod UID. Every future invocation
checks that marker after acquiring the volume lock and refuses to start celld for
that UID. Markers remain across successive retained-volume reuse, so a delayed
old kubelet cannot resurrect an earlier writer. The signed `RestartDenied` bit
is set only after this durable write. A marker supplies negative startup authority
only: it never reconstructs a positive stop receipt.

If the launcher crashes before the controller
records this response, no persisted file can recreate it. An unrequested child
exit does not automatically restart or certify an operation.

Trust assumptions: the pinned launcher/runtime and native subprocesses do not
close or unlink the inherited lock while retaining disk access; workloads cannot
compromise those native processes or read/forge the launcher credential; and
namespace/storage administrators do not rewrite authority or bypass attachment
fencing. A local lock does not fence a different kernel, unsafe EBS force-detach,
or a copied filesystem. These events remain outside the supported path.

Every operation-bearing request must carry an expiry between now and ten seconds
ahead, in every phase; the controller sends at most three seconds, bounded by
the operation deadline. A new operation binds only to a `Running` child. A
launcher that is `Terminating` because kubelet signaled it, or that already
exited or stopped without an operation, refuses later bindings, so an
unrequested exit can never be certified for an operation. Identity files are
written to a private name and linked into place; an empty identity file is
reported as corruption rather than read as another host. While the launcher is
PID 1 it reaps orphaned descendants, never the supervised child itself.

## Controller sequence

1. Validate the owned StatefulSet, exact runtime/init invocation, complete ordinal
   membership, healthy host incarnation, container identity, retained PVC/PV
   bindings, and authenticated launcher generations against a complete direct
   S3 inventory. Require full fresh survivor metrics and conservative donor
   demand projection. Preserve strict AZ/hostname constraints after removal.
2. Persist all candidate invocations and peer-log epochs. Persist `Stopping`
   before issuing the authenticated stop to the highest ordinal. Revalidate
   before sending the command; mode changes and insufficient fresh automatic
   stabilization block an unissued automatic command.
3. Require the exact launcher's live `Stopped` response. The StatefulSet still
   has its original replica count, so its controller cannot create a replacement
   merely because celld stopped; the supervisor remains running.
4. Require the donor's matching expired lease and sealed log (or a positively
   observed no-log session that never had a captured log). Reject disappeared
   logs, epoch rewind, changed generations, partial inventory and any loss marker.
5. Require each survivor to exclude the donor from its current ensemble. If its
   captured ensemble depended on the donor, require a later epoch or sealed log.
   The pin's [reconfiguration barrier](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/node_log.rs#L5204-L5254)
   first covers outstanding shipped writes in the bucket before abandoning old
   fragments. This addresses follower obligations separately from the donor's
   own log. A new independent survivor failure invalidates the assessment.
6. Persist positive retirement authority, then issue exactly one resourceVersion
   CAS decrement. Lost responses reconstruct the same operation. Recheck complete
   metadata/loss evidence and unchanged healthy survivors through ten seconds of
   settling before marking the retirement complete.
7. On growth, admit only positively retired historical generations. Keep the new
   pod scheduling-gated while checking the PVC UID, PV UID and CSI volume handle.
   Legacy/local disks remain pinned to the old Node UID/boot ID. Transferable
   production disks are pinned to their original AZ. A different host/boot waits
   for the authenticated transfer described below before celld can start.
   Keep the operation in `Reactivating` until the new launcher is Running, celld
   is Ready, and its new generation has a live matching node record with no loss
   declaration. Preserve all historical retirement authority.

A controlled stop already committed to `Stopping` is an in-flight operation;
compatible additions cannot silently cancel it. Pause retains its state. An
unissued Intent/Blocked operation retains the existing workload-CAS cancellation
mechanism. Already stopped/recovering operations remain unresolved on missing
proof; neither deadlines nor restored desired counts manufacture success.

A `Stopping` operation whose launcher still reports `Running` is provably
unissued once every stop request for it has expired: requests carry an expiry
no later than the operation deadline, and the launcher rejects expired ones
before changing phase. One minute after the deadline (a clock-skew margin) the
controller therefore cancels it through the same workload-CAS fence as an
Intent-phase removal and records `CanceledBeforeIssue`. A launcher reporting
`Stopping`, `Stopped`, an unrequested exit or a blocked lock is never canceled.

In steady state, with no operation in flight, the controller admits every
running invocation into the retained history: pod UID, container, launcher
invocation and generation, Node UID, boot ID, provider ID, zone, disk nonce and
PVC/PV/volume identity. Admission is observational and never resolves an earlier
entry; a running generation that follows an unresolved admitted writer on the
same ordinal blocks with `PersistentAdmissionBlocked` for investigation. The
record is what a later uncertain failure can be fenced against.

A survivor whose pod is recreated without any operation (eviction, kubelet
restart) may return only onto its recorded host incarnation and retained volume:
Node UID, boot ID, hostname, PVC UID, PV UID and volume handle must be unchanged
and no pod with the previous UID may remain. The controller records the old
invocation as `Superseded` before releasing the scheduling gate. Exclusion
authority for that return is the successor launcher's exclusive lock; the
superseded generation counts as resolved history once its lease has expired and
its log is sealed or absent, the same evidence required of a retired donor minus
the stop receipt that no unrequested replacement can have. A changed host or
volume identity keeps the gate closed and the fleet reports
`PersistentSchedulingBlocked`.

## Supported and blocked paths

- Experimental **manual graceful contraction from at least three to at least two**
  is executable for launcher-managed fleets, as is repeated retained-disk growth
  on the same host or, under the explicit EBS contract, another host in the same AZ. Production automatic contraction remains explicitly release-gated;
  the disposable local fixture runs the same automatic execution path.
- **Two to one is blocked before stopping any child.** With no remaining peer,
  the runtime may never publish an ensemble excluding its last follower. The
  current private metadata lacks an externally observable bucket-tiering watermark
  to authorize that retirement. Supporting it requires another concrete runtime
  evidence contract; launcher termination alone is insufficient.
- Unexpected launcher/container generations, loss markers, missing historical
  authority, cross-AZ reuse, unqualified storage handoffs, and changed storage
  bindings remain blocked. An uncertain node failure of an admitted member is
  reported, never repaired, unless an administrator authorizes the
  [exact-instance recovery](infrastructure-fencing.md#recovering-an-unreachable-member);
  a member that was never admitted stays pinned to its lost host. There is no
  force-delete/detach or administrator-written completion switch: the only pod
  the operator removes without a grace period is one whose instance EC2 has
  positively reported terminated.
- Production requests use `ReadWriteOncePod` on EBS CSI. The operator expects the
  externally provided CSI driver to support it. The local test mode uses RWO
  hostPath provisioning solely to exercise same-host semantics; it is not an EBS
  or CSI qualification substitute.
- Existing RWO/non-launcher fleets do not silently migrate. Enabling the image
  flag makes their old workload template differ and blocks reconciliation. Bound
  claim modes are never mutated or foreign claims adopted. An in-place migration
  requires a separately designed and qualified maintenance path; a fresh fleet
  uses fresh exclusive claims and its own bucket reservation.
- The retained journal preserves version 6 authority alongside newer fields and uses [immutable archives](journal-archives.md) beyond 180 KiB (16 MiB hydrated limit).
  Older binaries cannot downgrade or discard the new lifecycle authority.

See [local evidence](qualification/persistent-launcher/README.md). AWS IAM/S3/EBS,
CSI/RWOP behavior, correlated AZ failure and platform disruption remain separate
qualification work. Planned upgrades and final fleet deletion remain separate
implementation work.

Credential bootstrap first records a durable random creation nonce in the
reservation. It can resume an interrupted immutable Secret creation only when
the Secret carries that exact nonce and fleet identity; it then pins the key
digest. An existing unrelated Secret is never silently adopted.

## Graceful cross-node volume handoff

This is an executable controller/launcher protocol, not proof that a cloud test
passed. Production cross-node handoff requires EBS CSI filesystem RWOP claims,
retained delayed-binding StorageClass with explicit `type: gp2` or `type: gp3`,
and unchanged PVC/PV/CSI handle. Types permitting EBS Multi-Attach and unknown
storage classes are rejected. Static or administrator-modified storage outside
that contract is unsupported. Operators never force-delete or detach a volume.

Each new launcher creates and fsyncs a random disk nonce before starting celld.
The nonce travels in signed responses and retirement history. A launcher on a
different host or boot acquires the local lock but stays `WaitingForHandoff`;
it has not spawned celld. The operator requires the latest predecessor to be
positively stopped and retired, unchanged disk nonce/bindings, the same AZ,
healthy target boot, and fresh S3 evidence that the predecessor is still expired
and sealed (or its positively captured no-log session remains no-log). Loss,
revived lease, stale generation, or incomplete recovery blocks authorization.

The CSI attachment list must contain exactly one healthy, nondeleting EBS
attachment, to the destination. Any old attachment, attachment/deletion error,
missing attachment, or additional target blocks. This is attachment continuity,
not process-death evidence: the latter was already recorded from the exact live
supervisor before retirement. Namespace/storage administrators must not bypass
CSI fencing, copy volumes, or rewrite authority.

The operator sends an HMAC-authenticated grant expiring within three seconds,
bound to the destination Pod UID, launcher invocation, runtime generation, host,
boot, disk nonce and old host stamp. The waiting launcher checks every field,
atomically replaces/fsyncs the host stamp, waits restart spacing, then spawns the
unchanged celld binary. Delayed grants cannot start a different invocation.
Crashing before the grant leaves the disk blocked; crashing after the stamp
commit cannot reconstruct positive predecessor termination or replay authority
on another host. The same-host inherited lock still protects surviving children.

Version 6 journals remain readable, but historical retirement receipts without
signed `RestartDenied` authority cannot authorize reuse or excuse old writers.
Older launchers did not enforce persistent negative startup authority and could
resurrect a stopped Pod UID after a container restart. A legacy retired history
therefore requires a separately qualified migration/fencing path, not merely a
host match or a disk nonce. The operator never infers termination or nonresurrection
from file markers. Active legacy invocations likewise must move to the qualified
launcher before their stop can supply the new receipt.
