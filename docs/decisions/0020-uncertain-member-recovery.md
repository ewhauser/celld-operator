# ADR 0020 Uncertain PersistentFleet member recovery through exact-instance fencing

Status: Implemented experimental path; AWS qualification pending

Date: 19 September 2026

An admitted PersistentFleet member whose host incarnation disappears outside any
operation previously left the fleet permanently degraded: the recreated ordinal
stayed pinned to the lost host and no authority could prove that the old
process had lost access to its retained disk. [ADR 0017](0017-persistent-launcher-and-graceful-retirement.md)
deliberately left uncertain node death blocked, and the opt-in EC2 fence of
[infrastructure fencing](../infrastructure-fencing.md) covered only a
contraction donor captured while it was already stopping.

Decision: generalize that fence into a general recovery, without weakening it.

1. The controller admits every running invocation into the retained history in
   steady state, so the exact pod, launcher generation, host incarnation,
   instance, disk nonce and volume exist before a failure. Admission is
   observational: it never resolves, supersedes or retires an entry, and a
   running generation that follows an unresolved writer blocks for review.
2. An admitted member is uncertain only when its Node is gone, re-registered or
   rebooted, or NotReady while its launcher is unreachable. A launcher that
   answers is never uncertain. Uncertainty is reported, never repaired.
3. An administrator authorizes fencing one exact invocation by annotation. The
   fence keeps every identity check of the contraction path. Two relaxations
   apply only to recovery: an instance EC2 already reports `terminated` with
   matching tags is a receipt without a termination call, and a missing Node
   registration does not block, because the instance is named by ID and tags.
   Before durable intent the request can be withdrawn and a returning launcher
   refuses it; after intent it completes.
4. Positive termination is the stop and restart-denial authority for the retired
   invocation. The dead pod is removed without a grace period only after that
   receipt. Reactivation reuses the existing zone-pinned scheduling, signed
   handoff grant, sealed-predecessor and exclusive-attachment checks unchanged.

Rejected alternatives: inferring death from Node conditions, pod absence, lease
expiry, a changed boot ID or a CSI detach (none proves the old kernel lost the
disk); accepting `InvalidInstanceID.NotFound` as termination (EC2 eventual
consistency); and automatic fencing without an administrator request
(termination is destructive and the dedicated-node contract is external).

Journal version 9 carries the recovery record; older binaries reject it. Fake EC2
and attachment tests cover the sequence, crash points at every save, unsafe
refusals and journal validation. Real node loss on EKS, EBS reattachment and the
peers' seal timing remain [release gates](../qualification/eks-smoke-plan.md).
