---
title: Run celld fleets on Kubernetes
description: Install celld operator, deploy your first fleet, and manage scaling and maintenance on Kubernetes.
tableOfContents: false
hero:
  title: Run celld fleets on Kubernetes
  tagline: Define a fleet in YAML. Let the operator manage its workloads, storage connections, scaling, and maintenance.
  actions:
    - text: Install the operator
      link: ./start/install/
      icon: right-arrow
    - text: Choose a storage profile
      link: ./configure/profiles/
      variant: secondary
    - text: Operate a fleet
      link: ./operate/scaling/
      variant: minimal
---

:::note[Experimental — for evaluation]
AWS deployment and failure testing are still incomplete. Review [capabilities and limitations](./reference/limitations/) before choosing a workload for evaluation.
:::

## From a manifest to a working application

[celld](https://github.com/denoland/celld) runs stateful JavaScript applications using durable cells. The operator manages the Kubernetes fleet that hosts them. You supply the cluster, an S3 bucket, and AWS permissions; the operator creates the workloads, Services, and network policies.

1. [Prepare your cluster](./start/prerequisites/) and its storage and permissions.
2. [Install the operator](./start/install/) and [create a fleet](./start/first-fleet/).
3. [Run your first application](./start/first-application/) and check its response.

## Already running a fleet?

| I want to… | Start here |
| --- | --- |
| Add or remove replicas | [Scale a fleet](./operate/scaling/) |
| Respond to changing demand | [Enable capacity policy](./operate/capacity/) |
| Restart or upgrade | [Restart a fleet](./operate/restart/) · [Upgrade the runtime](./operate/upgrade-runtime/) |
| Check health or investigate a problem | [Monitor a fleet](./operate/monitoring/) · [Troubleshoot](./troubleshoot/) |
| Stop a fleet and understand what remains | [Delete a fleet and retain data](./operate/deletion/) |

## Choose storage for your workload

**Bucket** uses S3 for durable writes and temporary local disk. **PersistentFleet** adds retained EBS volumes and peer-disk durability. The choice affects setup, maintenance, and recovery: [compare the profiles](./configure/profiles/) before creating a fleet.

For configuration fields, see the [Fleet API](./api/celldfleet/) and [Helm values](./api/helm-values/). To work on the operator itself, start with [Contribute](./contribute/).
