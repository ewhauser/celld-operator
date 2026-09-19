---
title: AWS identities
description: Attach separate IAM roles for the celld runtime writer and the operator's recovery evidence reader.
sidebar:
  order: 2
---

The operator creates no AWS bucket, role, node or physical EBS volume. Provision the bucket, roles, nodes and EBS CSI driver before creating a fleet; the operator creates PersistentFleet PVCs that ask CSI to provision volumes. The operator and celld must use **different Kubernetes ServiceAccounts and IAM roles**.

| Identity | Kubernetes location | AWS access |
| --- | --- | --- |
| Runtime writer | `spec.serviceAccountName` in the fleet namespace | Runtime's required S3 reads and writes for its dedicated bucket |
| Evidence reader | `celld-system/celld-celld-operator` for Helm release `celld` | List the dedicated bucket's nodes/ and log/ metadata, and read node metadata objects; no writes or application data |
| Optional fencing | Operator credentials, only if both EC2 fencing flags are set | Restricted DescribeInstances and TerminateInstances for dedicated, tagged instances; see [fencing contract](../../contracts/infrastructure-fencing/) |

Use one bucket per fleet. Do not assign the operator the runtime writer role. The reader code constructs an AWS S3 client from the operator Pod's identity and the fleet's explicit region; it reads recovery metadata under nodes/ and log/. The runtime Pod gets its identity from `spec.serviceAccountName`, not from the operator's ServiceAccount.

## Attach roles with EKS Pod Identity

Ensure the [EKS Pod Identity Agent](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html) is present (it is built into EKS Auto Mode). Its node role needs eks-auth:AssumeRoleForPodIdentity, and private-subnet nodes need access to the EKS Auth API. Create two IAM roles with the [Pod Identity trust policy](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-role.html). Attach a scoped writer policy to the runtime role and a separate scoped reader policy to the operator role. Then associate the existing service accounts:

~~~sh
aws eks create-pod-identity-association \
  --cluster-name CLUSTER \
  --region REGION \
  --namespace FLEET_NAMESPACE \
  --service-account celld-runtime \
  --role-arn arn:aws:iam::ACCOUNT:role/celld-runtime-writer

aws eks create-pod-identity-association \
  --cluster-name CLUSTER \
  --region REGION \
  --namespace celld-system \
  --service-account celld-celld-operator \
  --role-arn arn:aws:iam::ACCOUNT:role/celld-evidence-reader
~~~

Replace the placeholders and ensure each ServiceAccount exists. REGION is the EKS cluster and bucket region. The runtime ServiceAccount is yours to create; Helm release celld creates celld-system/celld-celld-operator. A different Helm release produces a different operator ServiceAccount name; inspect the rendered chart before associating it. If you install the plain manifests, use celld-system/celld-operator instead. AWS documents the [association command](https://docs.aws.amazon.com/cli/latest/reference/eks/create-pod-identity-association.html) and [identity setup](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-association.html).

After creating an operator association **after Helm install**, restart its Deployment so new Pods receive the EKS-injected credential environment and projected token:

~~~sh
kubectl --context YOUR_CONTEXT -n celld-system rollout restart deployment/celld-celld-operator
kubectl --context YOUR_CONTEXT -n celld-system rollout status deployment/celld-celld-operator
~~~

[EKS Pod Identity injects credentials at Pod creation](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-how-it-works.html). The restart is unnecessary when an association exists before the operator Pods are created.

Alternatively use [IRSA](https://docs.aws.amazon.com/eks/latest/userguide/associate-service-account-role.html): annotate the runtime ServiceAccount with its writer role ARN, and set the chart's serviceAccount.annotations.eks.amazonaws.com/role-arn to the reader role ARN. IRSA also requires the cluster OIDC provider and a trust policy bound to the exact namespace and ServiceAccount.

## Runtime writer policy shape

The pinned [celld v0.5.0 bucket client](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/celld/bucket.rs) lists, reads, writes and deletes objects and can begin multipart uploads; its [replica client](https://github.com/denoland/celld/blob/12d5b6333fe52717325addcfe1e99e9fd4f77bcd/crates/ltx/src/client/object_store.rs) uses multipart upload for larger files. For a bucket dedicated to one fleet, this is a practical starting policy for the **runtime role only**:

~~~json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ListDedicatedBucket",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::FLEET_BUCKET"
    },
    {
      "Sid": "ReadWriteDedicatedBucketObjects",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": "arn:aws:s3:::FLEET_BUCKET/*"
    }
  ]
}
~~~

[S3's action mapping](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-with-s3-policy-actions.html) covers these object and multipart operations. Do not attach this policy to the operator role. If you use a customer-managed KMS key, separately grant the runtime the required key operations and verify its key policy; the example assumes ordinary S3 bucket encryption. An application deployment from your workstation also needs separate write permission for that bucket.

## Reader policy shape

This example grants only the evidence paths for one bucket. Keep the runtime writer's separate policy aligned with the pinned celld runtime's actual S3 operations; do not copy this reader policy onto the runtime.

~~~json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ListEvidence",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::FLEET_BUCKET",
      "Condition": {
        "StringLike": {
          "s3:prefix": ["nodes/*", "log/*"]
        }
      }
    },
    {
      "Sid": "ReadEvidence",
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": [
        "arn:aws:s3:::FLEET_BUCKET/nodes/*"
      ]
    }
  ]
}
~~~

Check the final policy against your bucket policy, KMS key policy if applicable, and the [S3 evidence contract](../../history/s3-recovery-evidence/). An explicit deny elsewhere still takes precedence. Do not put access keys in the CelldFleet manifest.

## Confirm the attachment

~~~sh
kubectl --context YOUR_CONTEXT -n FLEET_NAMESPACE get serviceaccount celld-runtime
kubectl --context YOUR_CONTEXT -n celld-system get serviceaccount celld-celld-operator
aws eks list-pod-identity-associations --cluster-name CLUSTER --region REGION
~~~

For IRSA, inspect each ServiceAccount's role annotation instead. Confirm the two role ARNs differ. Then follow [first fleet](../../start/first-fleet/) and [verify](../../start/verify/). These setup steps do not establish AWS qualification for lifecycle behavior.
