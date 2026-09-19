---
title: Architecture
description: How a CelldFleet connects Kubernetes workloads, storage, evidence and lifecycle decisions.
sidebar:
  order: 1
---

A CelldFleet is the desired state for one independent celld cluster. The operator watches that resource, creates the supporting Kubernetes objects, and reports what it can actually prove about the fleet. One operator installation can manage fleets in multiple namespaces; each fleet owns a dedicated bucket and a permanent cluster-scoped storage reservation.

![Clients send requests through the application Service to celld pods. The operator manages pods and records operations in Kubernetes. celld writes to S3 and optional retained EBS disks; the operator only reads S3 recovery metadata.](../../../assets/architecture.svg)

The runtime handles application requests and durable data. The operator handles placement, observed health, scaling and planned lifecycle actions. S3 evidence tells it whether older sessions and peer logs are still relevant; for PersistentFleet, launcher receipts and volume identity add another stop-and-attachment proof. The operator never writes S3 data.

## What the operator creates

| Resource | Purpose |
| --- | --- |
| Deployment or StatefulSet | Runs the chosen [profile](../profiles/) |
| Application ClusterIP Service on 8080 | Stable address for labelled client Pods |
| Headless peer Service on 8081 | Peer discovery |
| NetworkPolicy | Restricts client, peer, launcher and egress paths |
| PodDisruptionBudget | Prevents voluntary disruptions while the operator coordinates lifecycle |
| CelldStorageReservation | Permanently binds the bucket to one fleet UID and holds lifecycle authority |
| Retained PVCs and launcher credential | PersistentFleet only |

The operator does not create AWS buckets, IAM roles, node groups, ingress, TLS or DNS. Prepare those through [AWS identities](../../configure/aws/), [storage](../../configure/storage/), [networking](../../configure/networking/) and [placement](../../configure/placement/).

## One reconciliation loop, two decisions

Observation gathers Pod inventory, runtime /state, S3 recovery evidence and Metrics Server samples. Incomplete observation is invalid, not zero. Capacity policy may recommend a replica count, but the lifecycle executor decides whether the required evidence permits each action. It writes intent to the [journal](../lifecycle-journal/) before changing a workload or sending a stop request. Conditions and status show the result; [safety model](../safety-model/) explains why an action may remain blocked.

The image contains the celld-operator controller and a celld-launcher used by PersistentFleet. The launcher holds the volume lock, seeds the runtime lease generation and serves authenticated stop requests. Read the [PersistentFleet lifecycle](../../contracts/persistent-fleet-lifecycle/) for its exact contract. The [architecture decision](../../decisions/0001-controller-architecture/) records design history.
