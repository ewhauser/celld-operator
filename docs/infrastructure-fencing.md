# Opt-in EC2 fencing

This experimental path completes a PersistentFleet contraction whose admitted
writer became unreachable during `Stopping`. It terminates a dedicated EC2
instance, waits for a positive `DescribeInstances` `terminated` result, then
resumes the existing sealed-log, follower-retirement, retained-disk, and CSI
handoff checks. Termination alone never declares recovery complete.

It requires a member admitted by this operator version while its launcher and
host were healthy. The admission records the Pod UID, invocation/generation,
Node UID, boot ID, provider ID, disk nonce, PVC/PV UIDs and EBS volume ID. An
unrecorded failure, a replacement Node, or a new instance behind the same host
name cannot acquire fencing authority. This is not a generic node-repair loop.

Enable both `--ec2-fencing-account=123456789012` and
`--ec2-fencing-region=us-east-1` on the operator. Local test mode rejects this
configuration. Helm exposes the same opt-in as `ec2Fencing.account` and
`ec2Fencing.region`; both default to empty. The AWS SDK uses the operator's normal workload credentials;
celld's storage credentials are not used. Only standard commercial AWS regional
EC2 endpoints are supported. No custom endpoint is accepted for fencing proof.

Provision dedicated nodes and use trusted infrastructure automation to set these
instance tags after Kubernetes registration:

| Tag | Required value |
| --- | --- |
| `celld.eric.dev/fleet-uid` | exact CelldFleet UID |
| `celld.eric.dev/node-uid` | exact Kubernetes Node UID |
| `celld.eric.dev/boot-id` | exact admitted Node boot ID |
| `celld.eric.dev/fencing` | `terminate` |

The operator never adds these tags. Protect tag mutation and Pod scheduling on
these nodes with infrastructure IAM/admission controls. A reboot changes the
binding and is not silently adopted. Nodes must contain only the selected fleet
Pod and kube-system DaemonSets; another workload blocks termination. The operator
cordons the bound Node before issuing termination. Its data EBS mapping must have
`DeleteOnTermination=false`; any additional non-root data disk blocks the action.
Use taints/admission controls to enforce dedicated-node placement; privileged
actors that bypass scheduling/cordoning remain outside this trust boundary.

Authorize the **specific** stalled operation by setting the fleet annotation
`celld.eric.dev/fence-operation` to its lifecycle operation ID. This annotation
is a destructive-action request, never proof that fencing happened. A durable
intent is saved before any EC2 mutation. Once recorded, reconciliation completes
that intent rather than switching back to graceful same-host reuse. Removing the
annotation stops further requests but does not undo a request already submitted.
Remove it after completion. The default is no infrastructure termination.

Scope the operator role to one account/region and eligible dedicated fleet:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*",
      "Condition": {"StringEquals": {"aws:RequestedRegion": "us-east-1"}}
    },
    {
      "Effect": "Allow",
      "Action": "ec2:TerminateInstances",
      "Resource": "arn:aws:ec2:us-east-1:123456789012:instance/*",
      "Condition": {"StringEquals": {
        "ec2:ResourceTag/celld.eric.dev/fencing": "terminate",
        "ec2:ResourceTag/celld.eric.dev/fleet-uid": "REPLACE_WITH_FLEET_UID"
      }}
    }
  ]
}
```

Do not grant CreateTags, AttachVolume, DetachVolume, DeleteVolume, or instance
creation permissions. The operator does not force termination, disable termination
protection, or force-detach EBS. It retries exact instance IDs. `shutting-down`,
`stopped`, an API timeout, missing instance, Pod deletion, and expired leases are
not fences. AWS documents that even `InvalidInstanceID.NotFound` after termination
is not proof: [EC2 eventual consistency](https://docs.aws.amazon.com/ec2/latest/devguide/eventual-consistency.html).

The receipt survives leader crashes in the reservation journal (including its
immutable archive). A confirmed terminated-instance receipt cannot authorize a
replacement runtime generation. Cross-node activation still requires the exact
retained disk, a fresh authenticated launcher challenge, and one healthy EBS CSI
attachment on an eligible node in the same AZ.

## Recovering an unreachable member

The same fence recovers an admitted PersistentFleet member whose host
disappeared outside any operation: the Node was deleted or re-registered, its
boot ID changed, or it is NotReady and the member's launcher cannot be reached.
A launcher that still answers with the admitted invocation is never uncertain,
whatever Kubernetes reports, because the kernel holding the volume lock is alive.

The operator admits every running invocation in steady state (journal v9), so
the exact identity to fence exists before the failure. When such a member becomes
unreachable the fleet reports `PersistentMemberUncertain`, names the invocation,
the instance and the required annotation, and does nothing else: the retained
disk stays bound, the recreated ordinal stays behind its scheduling gate, and
survivors keep serving. The message ends with a value of the form
`recover:<pod>:<launcher generation>`; set `celld.eric.dev/fence-operation` to
exactly that value to authorize fencing that invocation. Authorizing it for a
reachable member does nothing.

Once authorized the controller records a durable recovery record, then runs the
fence with the same identity checks as above: exact account, instance ID, zone,
fleet/node/boot tags, retained data volume and no `DeleteOnTermination`. The
ordinary sequence records intent, cordons any registration of that instance,
issues one `TerminateInstances` for the exact ID, and waits for a positive
`terminated`. Two differences apply to recovery only. An instance that
`DescribeInstances` already reports `terminated` with matching tags is accepted
as the receipt without a termination call, which is the common case after an
Auto Scaling replacement. And a missing Node registration does not block: the
instance is identified by ID and tags alone. Any other pod scheduled under that
node name, a running instance whose data volume is no longer attached, a loss
declaration, a paused or deleting fleet, or another operation in flight refuses
the fence. Before intent is recorded, removing the annotation withdraws the
request and a launcher that answers again refuses it; after intent, the request
completes as for a contraction.

With the receipt durable, the invocation is retired in history with the
terminated instance as its stop and restart-denial authority, bound to the
highest recovery epoch observed for its generation. The pod that belonged to the
terminated instance is deleted with a UID precondition and no grace period so
the StatefulSet can recreate the ordinal; it is the one pod removal the operator
performs without a launcher receipt, and only after positive termination. The
recreated pod is released with a zone selector, never a host selector, and its
launcher waits for the signed handoff grant. That grant still requires the
predecessor's lease to have expired and its log to be sealed by the surviving
peers, an exclusive healthy EBS CSI attachment to the destination, and the
unchanged disk nonce; a single-member fleet therefore cannot complete recovery
because nothing remains to seal the lost log. A stale `VolumeAttachment` for the
terminated node blocks until the attach/detach controller removes it, which
follows Node deletion. Completion admits the replacement generation with the
outcome `InfrastructureFencedMemberReactivated` and clears the record; while it
is in flight `status.lifecycle` shows the record's ID and phase (`Fencing`,
`Reactivating`) and no other lifecycle action starts.

Tests use fake EC2 responses only. Real EKS/IAM/EC2/EBS failure qualification is
still required before production use, including recovery of a lost node. Unsealed
all-stopped recovery and failures of members that were never admitted remain blocked.
