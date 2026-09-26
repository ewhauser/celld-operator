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
`maxUnavailable: 1`. An in-flight Bucket current operation is dropped with an
`OperationSuperseded` Event. Updating the budget needs the
PodDisruptionBudget `update` verb; reapply `config/rbac/fleet-namespace.yaml`
first if you manage that Role yourself.

PersistentFleets created by earlier releases also converge in place. The
operator switches the StatefulSet from `OnDelete` to `RollingUpdate` and writes
the current template, and the PodDisruptionBudget becomes `maxUnavailable: 1`.
Kubernetes then replaces each earlier Pod one at a time, highest ordinal first,
including Pods held by the launcher scheduling gate. Each member keeps its
claim, with the same name and identity. An in-flight strict operation is
dropped with an `OperationSuperseded` Event, and the reservation annotation is
rewritten to capacity-policy history alone, or removed. Launcher retirement
markers on those disks are ignored. The `DiskCleanupPending` condition is
removed and `status.lifecycle` is left empty. See the
[PersistentFleet lifecycle](../../concepts/current-operation/).

This release needs fewer permissions. The ClusterRole no longer grants
PersistentVolume, Node or VolumeAttachment reads, and the fleet-namespace Role
no longer grants PVC `create`. The Role keeps Pod and PVC `delete`, which the
operator uses to heal members. The chart applies the smaller roles; if you
apply `config/manager/operator.yaml` or `config/rbac/fleet-namespace.yaml`
yourself, reapply the release's copies.

This release heals PersistentFleets without an administrator. The operator
force-deletes a member Pod left on a node that no longer answers, in Ordered
Bucket fleets too. It replaces a member on a fresh disk when its volume is
gone, or when it is the one member down for the chart value
`memberReplacementDelay` (default `10m`, operator flag
`--member-replacement-delay`) while the rest of the fleet is ready. Apart from
image and configuration errors, it does not judge why a member is down: raise
the value before planned work that keeps one member down longer. See
[self-healing](../../concepts/current-operation/#self-healing).

The operator accepts `--launcher-image` and ignores it; `launcherImage` can be
dropped from chart values.

A binary downgrade is not a storage or protocol rollback procedure: an earlier
release expects launcher-supervised PersistentFleet Pods.
