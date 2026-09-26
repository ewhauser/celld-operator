---
title: Upgrade the operator
description: Apply matching CRDs and chart, and let existing fleets converge.
---

This implementation replaces the former lifecycle journal and private-S3
controller. There are no deployed-user migration requirements and no
compatibility path for old authority. Create fresh fleets with the
current API and dedicated buckets.

When upgrading from v0.0.2, remove `spec.qualification` from saved fleet
manifests. Apply the new CRD before the chart; the controller clears the old
`ProductionQualified` and `LifecycleBlocked` status conditions as fleets reconcile.

For subsequent compatible releases, verify the exact controller artifact
against the runtime fork before rollout. Apply the release's CRDs
explicitly, because Helm does not update CRDs from its `crds/` directory, and
install the matching chart:

```bash
kubectl --context YOUR_CONTEXT apply -f config/crd/
helm --kube-context YOUR_CONTEXT upgrade celld charts/celld-operator   --namespace celld-system --reuse-values   --set image.digest=sha256:YOUR_OPERATOR_DIGEST
```

The controller declares `appProtocol: tcp` on each fleet's peer Service port.
Peer Services created by earlier releases lack it; the controller fills that field
in place when the Service otherwise matches exactly. That needs the Service
`update` verb in the fleet-namespace Role, which the chart's Role includes. If you apply `config/rbac/fleet-namespace.yaml`
yourself, reapply the release's copy in every fleet namespace before the chart;
until then fleets report `InfrastructureBlocked` naming that file. A peer Service
with any other difference, including a different `appProtocol`, stays blocked.
Rolling back to an earlier controller reports the new field as drift.

Bucket fleets created by earlier releases converge in place: one rolling update
removes the launcher from the Pod template and switches the workload to
one-member rolling updates, and the PodDisruptionBudget becomes
`maxUnavailable: 1`. An in-flight Bucket current operation is dropped and
recorded with outcome `Superseded`. Updating the budget needs the
PodDisruptionBudget `update` verb; reapply `config/rbac/fleet-namespace.yaml`
first if you manage that Role yourself.

PersistentFleets created by earlier releases also converge in place. An
in-flight strict operation is recorded as `Superseded`, unreadable state is
rebuilt, launcher-gated Pods are released, and members roll onto the new template
one at a time on their existing disks, each after the fleet settles. Launcher
retirement markers on those disks are ignored. The `DiskCleanupPending`
condition is removed and `status.lifecycle` no longer shows operation phases.
The operator accepts `--launcher-image` and ignores it; `launcherImage` can be
dropped from chart values.

A binary downgrade is not a storage or protocol rollback procedure: an earlier
release expects launcher-supervised PersistentFleet Pods.
