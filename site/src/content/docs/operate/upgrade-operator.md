---
title: Upgrade the operator
description: Use matching API, controller and launcher artifacts.
---

This implementation replaces the former lifecycle journal and private-S3
controller. There are no deployed-user migration requirements and no
compatibility path for old authority. Create fresh fleets with the
current API and dedicated buckets.

When upgrading from v0.0.2, remove `spec.qualification` from saved fleet
manifests. Apply the new CRD before the chart; the controller clears the old
`ProductionQualified` and `LifecycleBlocked` status conditions as fleets reconcile.

For subsequent compatible releases, verify the exact controller and launcher
artifacts against the runtime fork before rollout. Apply the release's CRDs
explicitly, because Helm does not update CRDs from its `crds/` directory, and
install the matching chart:

```bash
kubectl --context YOUR_CONTEXT apply -f config/crd/
helm --kube-context YOUR_CONTEXT upgrade celld charts/celld-operator   --namespace celld-system --reuse-values   --set image.digest=sha256:YOUR_OPERATOR_DIGEST   --set launcherImage=YOUR_REGISTRY/celld-operator@sha256:YOUR_OPERATOR_DIGEST
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

Inspect PersistentFleet current operations and the release's workload-template
compatibility before changing the launcher image. The operator does not silently
roll out a drifted PersistentFleet template. Do not erase current authority or force a workload update
to work around a blocker.

A controller restart resumes captured current operations; status can be rebuilt.
A binary downgrade is not a storage or protocol rollback procedure.
