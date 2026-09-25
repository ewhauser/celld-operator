# One disruption at a time

Earlier releases drove PersistentFleet changes through a bounded current
operation: strict `remove-disk` proof, launcher exit and restart-denial proof,
a ten-phase executor and coordinated whole-fleet downtime for restart and
upgrade. That executor is removed. This page describes what replaced it.

celld tolerates the loss of any one member. In `fleet` durability a write is
acknowledged only after every follower in the leader's ensemble has fsynced it,
and a dead leader is recovered from one complete follower copy. The operator's
only safety job is therefore to disrupt one member at a time, wait until celld
reports that the fleet has absorbed it, and never delete an existing disk that a
session still needs. A disk that is already gone is replaced without waiting.

## Settlement

The operator reads celld's `/state.node_log` from every expected member (celld
`0.5.1-ewhauser.5` or later). The fleet is **settled** when:

- every expected member is Ready and reports `fleet` durability;
- every member has a follower ensemble (not required for a one-member fleet);
- a complete dead-leader sweep observed after the last disruption plus one
  lease TTL (10 seconds) lists no unrecovered session.

The last disruption is the later of the operator's own last change and any
member's latest Pod creation or container start. A member's disk is
**releasable** when a fresh complete sweep exists and none lists a session that
still needs that member. Anything unknown, including an unreadable member,
answers no. Waiting delays only the next voluntary step, never the running fleet.

## Lifecycle

| Change | Behavior |
| --- | --- |
| Restart, upgrade | `maintenance.restartToken` (Pod template annotation `celld.eric.dev/restart-token`) or `runtimeImage` rolls the template. The StatefulSet keeps `OnDelete`; the operator deletes outdated Pods one at a time: a member already down at once, running members highest ordinal first and only when settled. Disks are retained. |
| Scale-in | When settled, replicas drop by one (highest ordinal). Its PVC is deleted only once releasable; until then status names the sessions that still need it. Automatic and External contraction also require survivor-capacity evidence (`CapacityUncertain`). |
| Growth | New members get new claims. Growth waits until any removed member's retained PVC is deleted; it never reuses one. |
| Lost disk | A PVC in phase `Lost`, a bound PV that no longer exists, or a Pending Pod whose PVC is gone: the member's Pod and PVC are replaced at once. The status message and a `MemberDiskLost` Warning event name sessions celld may record as lost. |
| `celld.eric.dev/replace-member` | Annotation on the CelldFleet naming an ordinal or Pod name whose existing disk the administrator declares gone. The operator replaces Pod and PVC when settled, or at once if that member is the only one down, then removes the annotation and emits `MemberReplaced`. An unknown member reports `ReplaceMemberInvalid`. |
| Node drain | The PodDisruptionBudget allows `maxUnavailable: 1` while settled and `0` while recovering. |
| Deletion | Foreground StatefulSet deletion (members drain on SIGTERM), then every fleet PVC, then the finalizer. The bucket reservation is permanent. |

celld seals a session whose only complete copy was on a lost disk with a bounded
loss record, so the fleet never waits on a disk that is gone. The operator never
deletes an existing disk with outstanding obligations on its own; only a lost
disk or an explicit `replace-member` request does that.

Runtimes without node-log state (for example `0.5.1-ewhauser.4`) report
settlement as unknown. Restarts and upgrades may still roll on readiness plus a
one-minute stabilization after the last disruption, so an upgrade from `.4` to
`.5` works. Such runtimes never authorize disk deletion: scale-in needs `.5`.

## Kubernetes objects

The operator converges drift in the StatefulSet template and replica count,
NetworkPolicy and PodDisruptionBudget it owns; Services stay verify-only. Objects
without the fleet's UID label, or with owner references, are refused. A missing
StatefulSet is recreated on the fleet's own claims: claims labeled with the fleet
UID are reused, and a foreign claim at a member's name reports
`StorageIdentityConflict`. StatefulSet PVC retention is `Retain` for scale and
deletion, so only the operator deletes claims.

The storage reservation's `celld.eric.dev/current-operation` annotation now
holds bookkeeping only: workload UID, applied count and image, last restart
token, last disruption time and capacity-policy state. It holds no runtime proof.

## Upgrading from the strict executor

An in-flight operation recorded by an earlier release is dropped with
`status.lifecycle.lastOutcome: Superseded`, and unreadable state is rebuilt
instead of blocking. Pods held by the launcher scheduling gate are released, and
members roll onto the new template one at a time under the rules above.
Launcher retirement markers on existing disks are ignored because no launcher
runs. The `DiskCleanupPending` condition is removed, and `status.lifecycle` no
longer shows operation phases.

## Regression coverage

`internal/fleethealth` covers settlement, stale sweeps, single-member fleets and
releasability. `internal/controller/persistent_test.go` covers provisioning
without the launcher, the budget, rolling restart on retained disks, down-member
updates, legacy runtimes, scale-in release, lost disks, `replace-member`,
workload recreation, deletion and migration from strict fleets. These are unit
tests with a fake client; see [qualification](qualification/README.md) for
cluster evidence.
