---
title: Prerequisites
description: Prepare an EKS cluster, a dedicated S3 bucket, runtime and operator AWS identities before installing.
sidebar:
  order: 2
---

This guide uses one nonproduction **Bucket** fleet named `my-fleet` in namespace `fleets`. Replace every `YOUR_…` value with your own account, region, cluster and bucket. Keep the bucket for this fleet alone; the operator does not release a bucket reservation when a fleet is deleted.

## Cluster and local tools

- Kubernetes **1.31 or newer**, IPv4 Pod networking and a CNI that enforces Kubernetes NetworkPolicy. Verify policy enforcement in your cluster before setting the operator's `networkPolicyEnforced` value. The value is an assertion, not a CNI installer.
- Worker capacity in every zone listed in the fleet spec. This guide uses three replicas across three zones; choose actual zones in your bucket's region.
- `kubectl`, Helm, Docker with Buildx, the AWS CLI, `curl`, `jq`, and permission to push an image to a registry your cluster can pull. Install `celld` v0.5.0 and `esbuild` on your workstation for the [application step](../first-application/).
- An explicit kubeconfig context. Check the cluster and account before changing anything:

```bash
kubectl --context YOUR_CONTEXT cluster-info
kubectl --context YOUR_CONTEXT get nodes -L topology.kubernetes.io/zone
aws sts get-caller-identity --profile YOUR_AWS_PROFILE
```

Create the fleet namespace before installing the chart. The chart can create `celld-system`; it does **not** create `fleets`.

```bash
kubectl --context YOUR_CONTEXT create namespace fleets
```

If the namespace already exists, verify it with `kubectl --context YOUR_CONTEXT get namespace fleets` instead.

## S3 and AWS identities

Create one dedicated S3 bucket in the selected region. Keep external writers out, and disable lifecycle expiry for `nodes/` and `log/` evidence. The bucket name and region go into the `CelldFleet`; credentials do not. Your organization may provision these with its normal infrastructure tooling.

Use **two different AWS roles**, both connected to Kubernetes ServiceAccounts through [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html) or [IRSA](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html):

| Identity | Kubernetes ServiceAccount | Bucket access |
| --- | --- | --- |
| Runtime | `fleets/celld-runtime` | Runtime read/write access to its dedicated bucket, including deployment objects. |
| Operator | `celld-system/celld-celld-operator` for the Helm release in this guide | Read `nodes/*` objects and list the `nodes/` and `log/` prefixes for lifecycle evidence. It must not write to S3 or read `log/` bundle bodies. |

The chart generates the operator ServiceAccount name from the Helm release name: release `celld` produces `celld-celld-operator`. If you use the plain manifest instead, it is `celld-system/celld-operator`. Configure the AWS association for the installation method you actually use. The chart's `serviceAccount.annotations` can hold an IRSA role annotation; EKS Pod Identity associations are managed outside the chart. See [AWS permissions](../../configure/aws/) for policy examples and association commands.

For Pod Identity, verify the agent is running and its node role can call `eks-auth:AssumeRoleForPodIdentity`; nodes in private subnets need access to the EKS Auth API. See [AWS agent prerequisites](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html).

Create the runtime ServiceAccount in `fleets` and connect it to the runtime role using your chosen EKS identity mechanism. For an existing Pod Identity association, a plain account is enough:

```bash
kubectl --context YOUR_CONTEXT -n fleets create serviceaccount celld-runtime
kubectl --context YOUR_CONTEXT -n fleets get serviceaccount celld-runtime
```

For IRSA, annotate that account with your runtime role ARN according to the [AWS IRSA guide](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html). Provision the operator Pod Identity association after Helm creates its ServiceAccount, then restart the operator Deployment and wait for its rollout so AWS injects credentials into new Pods. For IRSA, supply the role annotation with the Helm install. See [AWS identities](../../configure/aws/) for the exact commands and the separate runtime and reader policy examples. Your workstation's deployment credentials also need access to write the application into this same bucket; use an approved AWS profile and do not place static keys in Kubernetes manifests.

Next: [install the operator](../install/).
