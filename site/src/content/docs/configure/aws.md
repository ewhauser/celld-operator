---
title: AWS identities
description: Give bucket access to celld; the operator uses only Kubernetes credentials.
---

Provision a dedicated S3 bucket, runtime IAM role, worker nodes and, for
PersistentFleet, EBS CSI. The operator creates none of these AWS resources and
needs **no AWS IAM role**. It has no S3 reader or EC2 termination client.

| Identity | Purpose |
| --- | --- |
| Fleet runtime ServiceAccount | celld reads and writes its dedicated bucket. |
| Application deployer | Your workstation or pipeline writes deployments into that same bucket. |
| EBS CSI identity | The CSI driver provisions and deletes volumes under its own permissions. |
| Operator ServiceAccount | Kubernetes resource access from the shipped RBAC only. |

Create the runtime ServiceAccount named by `spec.serviceAccountName` and connect
it to a scoped role using [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html)
or [IRSA](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html).
For Pod Identity, prepare the agent and trust policy, then associate the existing
runtime account:

```sh
aws eks create-pod-identity-association   --cluster-name CLUSTER --region REGION   --namespace fleets --service-account celld-runtime   --role-arn arn:aws:iam::ACCOUNT:role/celld-runtime-writer
```

The runtime role needs the fork's S3 list, read, write, delete and multipart
operations for its dedicated bucket. Align permissions with the deployed
[fork bucket client](https://github.com/ewhauser/celld/blob/main/crates/celld/bucket.rs)
and object-store client; grant required KMS access separately when applicable.
Do not copy runtime write permissions onto the operator.

Keep one fleet as writer per bucket and avoid external expiry/deletion of
runtime metadata. Credentials belong in workload identity configuration, never
in the CelldFleet manifest. Verify the account, region, bucket policy and chosen
ServiceAccount before [creating the fleet](../../start/first-fleet/).
