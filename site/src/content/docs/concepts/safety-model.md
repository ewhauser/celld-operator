---
title: Safety model
description: What counts as evidence that a member can be retired, and why some actions remain blocked.
sidebar:
  order: 4
---

The operator treats removal as a proof obligation. A Pod can disappear while its process is still writing on a lost node; celld can also exit before all durability work is complete. The controller therefore identifies the exact admitted session and requires positive evidence before it retires that session or reuses its disk.

## Evidence by profile

| Evidence | Why it matters |
| --- | --- |
| Positively read expired S3 lease and no peer-log obligation | An old session no longer participates in recovery |
| Fresh healthy survivors through a settling interval | The remaining cluster has converged |
| Sealed own log | PersistentFleet can retire a prior writer without leaving unaccounted work |
| Launcher Stopped receipt and restart-denial marker | The exact PersistentFleet Pod UID cannot restart as the old writer |
| Matching retained PVC/PV/CSI identity and safe attachment | A replacement Pod uses the expected disk, in the expected zone |
| EC2 terminated result after an operator-issued termination | Optional fencing of one admitted unreachable PersistentFleet donor |

The operator persists observed evidence in the [journal](../lifecycle-journal/) so a later S3 cleanup or controller restart does not erase an established fact.

A missing Pod, exit code 0, Ready=True, wall-clock lease guess, retained PVC or force-detached EBS volume is **not** proof. A timeout never turns uncertain evidence into success. When proof is missing, the operator keeps the operation and storage, sets a blocker condition, and can still make compatible additive progress. See [conditions](../../reference/conditions/) for reason names and [troubleshooting](../../troubleshoot/) for response steps.

## What durability covers

Applications should tolerate interrupted requests and reconnect or retry as appropriate. The operator coordinates membership and retains acknowledged durable writes within its tested failure model; it does not preserve in-memory state or replay ambiguous non-idempotent requests. [Qualification evidence](../../qualification/) and [limitations](../../reference/limitations/) distinguish local tests from unrun AWS behavior. The [recovery-evidence decision](../../decisions/0008-recovery-evidence-and-conservative-removal/) records the underlying reasoning.
