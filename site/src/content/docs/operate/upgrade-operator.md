---
title: Upgrade the operator
description: Use matching API, controller and launcher artifacts.
---

This implementation replaces the former lifecycle journal and private-S3
controller. There are no deployed-user migration requirements and no
compatibility path for old authority. Create fresh evaluation fleets with the
current API and dedicated buckets.

For subsequent compatible releases, qualify the exact controller and launcher
artifacts against the runtime fork before rollout. Apply the release's CRDs
explicitly, because Helm does not update CRDs from its `crds/` directory, and
install the matching chart:

```bash
kubectl --context YOUR_CONTEXT apply -f config/crd/
helm --kube-context YOUR_CONTEXT upgrade celld charts/celld-operator   --namespace celld-system --reuse-values   --set image.digest=sha256:YOUR_OPERATOR_DIGEST   --set launcherImage=YOUR_REGISTRY/celld-operator@sha256:YOUR_OPERATOR_DIGEST
```

Inspect current operations and the release's workload-template compatibility
before changing the launcher image. The operator does not silently roll out a
drifted fleet template. Do not erase current authority or force a workload update
to work around a blocker.

A controller restart resumes captured current operations; status can be rebuilt.
A binary downgrade is not a storage or protocol rollback procedure.
