---
title: How removal is checked
description: Data safety, process exclusion and disk cleanup are separate proofs.
---

Removal passes three independent boundaries:

| Boundary | Required proof |
| --- | --- |
| Runtime | celld reports schema 1, exact operation and generation, `data_safe`, control-only mode and no blocker. |
| Process | The launcher captured that result before termination, observed its exact child exit, reacquired the inherited lock and durably denied restart. |
| Infrastructure | The controller persisted the bound result and conditionally changes only the captured workload, claim and volume identities. |

An HTTP 202 response means the runtime accepted a request. Exit code zero means
a process ended. Neither proves data safety. A deadline, expired lease or missing
Pod also cannot replace a positive strict result.

A launcher crash before Kubernetes captures completion may leave removal blocked
indefinitely. Its restart-deny marker is negative authority: it prevents a launch
but cannot recreate a positive runtime result. After proof is captured, the
controller can recover workload effects without contacting that launcher again.

The launcher blocks cross-host/boot reuse because local file locks cannot prove
exclusion across kernels. Growth uses fresh disks. No timeout, EC2 fence,
force-detach or storage-finalizer removal bypass exists.

Read [current operations](../current-operation/), [disk cleanup](../../contracts/disposable-disks/)
and [qualification limits](../../reference/limitations/). A ready fleet and a
passing local test are not production durability qualification.
