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
Pod and kube-system DaemonSets; another workload blocks termination. That check
lists Pods across every namespace (narrowed server-side by `spec.nodeName`), so
the ClusterRole in `config/manager/operator.yaml` grants `pods` `list` at cluster
scope. It is the only cluster-wide Pod verb, and without it fencing records its
intent and then fails `Forbidden` on every subsequent reconcile. The operator
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

Tests use fake EC2 responses only. Real EKS/IAM/EC2/EBS failure qualification is
still required before production use. Unsealed all-stopped recovery and failures
before exact admission remain blocked.
