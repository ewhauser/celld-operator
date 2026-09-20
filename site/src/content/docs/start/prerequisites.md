---
title: Prerequisites
description: Prepare a compatible fork, cluster, bucket and runtime identity.
---

Use a nonproduction cluster and a dedicated bucket for this experimental guide.
The example is an Ordered Bucket fleet named `my-fleet` in namespace `fleets`.

- Kubernetes 1.31 or newer with IPv4 Pod networking and an enforcing NetworkPolicy
  CNI. Independently verify policy enforcement before attesting it.
- Enough eligible worker nodes in the fleet's explicit zones. Three strictly
  placed replicas need three distinct hosts.
- A compatible fork runtime image and matching launcher/operator image, pinned
  by verified registry digests. All runtime/recovery nodes must use the fork.
  Stock upstream v0.5.1 is insufficient; see [compatibility](../../reference/compatibility/).
- `kubectl`, Helm, Docker/Buildx, AWS CLI, curl and jq. Use the fork's native CLI
  and esbuild for [application deployment](../first-application/).
- A dedicated S3 bucket and a runtime ServiceAccount with its bucket permissions.
  The operator needs no AWS IAM role. See [AWS identities](../../configure/aws/).

Select your context and account explicitly:

```bash
kubectl --context YOUR_CONTEXT cluster-info
kubectl --context YOUR_CONTEXT get nodes -L topology.kubernetes.io/zone
aws sts get-caller-identity --profile YOUR_AWS_PROFILE
kubectl --context YOUR_CONTEXT create namespace fleets
kubectl --context YOUR_CONTEXT -n fleets create serviceaccount celld-runtime
```

If the namespace or ServiceAccount already exists, inspect it instead of creating
it again. Attach the runtime identity using your EKS Pod Identity or IRSA setup.
Keep the bucket exclusive to this fleet, including its runtime metadata.

PersistentFleet additionally needs [supported CSI storage](../../configure/storage/).
Ordered Bucket uses temporary disks and does not require EBS. Both require the
launcher. Continue with [installation](../install/).
