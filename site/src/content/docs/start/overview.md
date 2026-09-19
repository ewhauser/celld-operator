---
title: Start here
description: Get from an empty EKS namespace to a ready celld fleet and your first application request.
sidebar:
  order: 1
---

celld operator runs a [celld](https://github.com/denoland/celld) fleet on Kubernetes. You provide an EKS cluster, a dedicated S3 bucket, AWS identities and a container image for the operator. A `CelldFleet` names the runtime ServiceAccount, bucket and zones. The operator creates the workload, internal Services and network policy. Your application then talks to the fleet's port 8080.

The path to a first request is:

1. [Check prerequisites](../prerequisites/) for your cluster, bucket and AWS identities.
2. [Build and install the operator](../install/) with an immutable image digest.
3. [Create a Bucket fleet](../first-fleet/) and wait for healthy runtime Pods.
4. [Deploy an application and send a request](../first-application/).
5. [Verify and troubleshoot](../verify/) the fleet when a condition stays false.

Start with a **Bucket** fleet. A PersistentFleet adds EBS CSI, a retained StorageClass and the launcher image; see [fleet profiles](../../concepts/profiles/) when you need retained disks. Each fleet runs one application from its own bucket. The operator does not create the bucket, IAM roles, worker nodes or public ingress.

:::caution[Experimental software]
The API requires `qualification: Experimental`. Cloud qualification is still outstanding and the operator reports `ProductionQualified=False`. Use a nonproduction cluster and bucket for this guide. See the [capability and qualification limits](../../reference/limitations/) before relying on a lifecycle operation.
:::
