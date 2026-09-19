---
title: Upgrade the operator
description: Prepare a Helm upgrade while preserving CRDs, reservations, and the lifecycle journal.
---

Upgrading the controller is separate from changing a fleet's celld runtime. The chart is experimental, and Helm does not upgrade its CRDs automatically. Confirm the target operator can read the current journal format before installing it; an older controller may refuse a newer journal.

## Prepare

Use the context for the target cluster. Record the release and save the fleet definitions and retained recovery objects to a restricted backup location. Reservation annotations can contain journal state, and launcher Secrets contain credentials; protect the export accordingly.

```bash
umask 077
helm --kube-context YOUR_CONTEXT -n celld-system list
kubectl --context YOUR_CONTEXT get celldfleets -A -o yaml > celldfleets.yaml
kubectl --context YOUR_CONTEXT get celldstoragereservations -o yaml > reservations.yaml
kubectl --context YOUR_CONTEXT -n fleets get configmaps,secret -o yaml > fleets-recovery-objects.yaml
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
```

Include every fleet namespace in the backup, not only `fleets`. Review the new CRDs and chart values, including `fleetNamespaces`, image digest, and `launcherImage`. Do not delete retained reservations or journal archive ConfigMaps as part of an upgrade. See [installation](../../start/install/), [compatibility](../../reference/compatibility/), and [journal archives](../../contracts/journal-archives/).

## Apply and verify

After reviewing the release's CRD changes, apply its CRDs explicitly and upgrade the chart from that same release:

```bash
kubectl --context YOUR_CONTEXT apply -f config/crd/
helm --kube-context YOUR_CONTEXT upgrade celld charts/celld-operator \
  --namespace celld-system --reuse-values \
  --set image.digest=YOUR_REVIEWED_IMAGE_DIGEST
kubectl --context YOUR_CONTEXT -n celld-system rollout status deployment/celld-celld-operator
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
```

The example uses a local checkout of the target release. Preserve the existing `launcherImage` pin for existing PersistentFleets: changing it changes their expected Pod template, which is reported as drift rather than automatically rolled out. There is no general launcher-template migration. Confirm the target controller supports that launcher and the existing templates before upgrading; if it does not, stop and review the migration requirements. `--reuse-values` preserves the pin. For a new installation only, the launcher normally uses the same image as the controller. Completion means the controller rollout is available and fleets reconcile without new blockers or journal read errors. Inspect operator logs and [installation troubleshooting](../../troubleshoot/installation/) if they do not. Do not assume a Helm rollback is safe after a journal version advances.
