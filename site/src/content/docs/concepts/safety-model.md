---
title: How removal is checked
description: celld tolerates one lost member; the operator disrupts one at a time and keeps disks a session still needs.
---

celld tolerates the loss of any one member. A Bucket fleet acknowledges no write
before S3 holds it. A PersistentFleet acknowledges a write only after every
follower in the leader's ensemble has fsynced it, and recovers a dead leader
from one complete follower copy. The operator's job is to keep voluntary
disruption to one member at a time.

| Check | PersistentFleet rule |
| --- | --- |
| Settled | Every member Ready in `fleet` durability with a follower ensemble, and a complete sweep after the last disruption plus one lease TTL lists no unrecovered session. |
| Next disruption | A running member is restarted, upgraded or removed only when settled. A member already down is replaced at once. |
| Disk release | A removed member's disk is deleted only when a fresh complete sweep lists no session that needs it. |
| Lost disk | A disk that is gone is replaced at once; celld recovers each session from another copy or records a bounded loss. |

Readiness, exit codes, expired leases and missing Pods are not settlement.
Unknown node-log state, including a runtime before `0.5.1-ewhauser.5`, delays
voluntary changes and never releases a disk. It never stops the running fleet.

The operator never deletes an existing disk with obligations on its own. An
administrator can declare one gone with the `celld.eric.dev/replace-member`
annotation; the operator then replaces that member under the same
one-disruption rule. No EC2 fence, force-detach or storage-finalizer removal
exists.

Read [one disruption at a time](../current-operation/),
[retained disks](../../contracts/disposable-disks/) and
[operational limits](../../reference/limitations/).
