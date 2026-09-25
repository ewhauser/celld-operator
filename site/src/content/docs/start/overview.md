---
title: Start here
description: Get from an empty EKS namespace to a ready celld fleet and your first application request.
sidebar:
  order: 1
---

celld operator runs a [celld](https://github.com/denoland/celld) fleet on Kubernetes. You provide an EKS cluster, a dedicated S3 bucket, a runtime AWS identity and digest-pinned fork and operator images. A `CelldFleet` names the runtime ServiceAccount, bucket and zones. The operator creates the workload, internal Services and network policy. Your application then talks to the fleet's port 8080.

The path to a first request is:

1. [Check prerequisites](../prerequisites/) for your cluster, bucket and AWS identities.
2. [Build and install the operator](../install/) with an immutable image digest.
3. [Create a Bucket fleet](../first-fleet/) and wait for healthy runtime Pods.
4. [Deploy an application and send a request](../first-application/).
5. [Verify and troubleshoot](../verify/) the fleet when a condition stays false.

Start with a **Bucket** fleet; it needs no CSI storage. A PersistentFleet adds retained CSI volumes and changes one member at a time; see [fleet profiles](../../concepts/profiles/) before selecting persistent local disks. Each fleet runs one application from its own bucket. The operator does not create the bucket, IAM roles, worker nodes or public ingress.
