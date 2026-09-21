---
title: Installation problems
description: Diagnose missing CRDs, namespace access, controller rollout, and external prerequisites.
---

Use the context where you installed the operator. The chart requires one operator installation per cluster and a namespace Role/RoleBinding for each fleet namespace. Helm does not upgrade CRDs automatically.

## Check the installation

```bash
helm --kube-context YOUR_CONTEXT -n celld-system status celld
kubectl --context YOUR_CONTEXT get crd celldfleets.celld.eric.dev
kubectl --context YOUR_CONTEXT -n celld-system get deployment,pods
kubectl --context YOUR_CONTEXT -n celld-system logs deployment/celld-celld-operator --tail=100
kubectl --context YOUR_CONTEXT -n fleets describe celldfleet my-fleet
```

If the CRD is missing, apply `config/crd/` from the same release as the controller and recheck. If the manager Pod is unavailable, use its Pod events and logs to distinguish image pull, configuration, and current-operation protocol errors. The development manifest's `:dev` image is not a published release image; supply an image your cluster can pull. [Install](../../start/install/) has the chart procedure.

`NamespaceAccessDenied` means the fleet namespace lacks its namespaced Role and RoleBinding. For the Helm release `celld`, inspect the current list and upgrade the chart with the **complete** set of fleet namespaces; setting `fleetNamespaces` replaces the previous list even with `--reuse-values`:

```bash
helm --kube-context YOUR_CONTEXT -n celld-system get values celld
helm --kube-context YOUR_CONTEXT upgrade celld charts/celld-operator \
  --namespace celld-system --reuse-values \
  --set 'fleetNamespaces={fleets}'
kubectl --context YOUR_CONTEXT -n fleets get rolebinding celld-celld-operator-fleet -o yaml
```

Add every other existing fleet namespace to the brace list before running the upgrade. The RoleBinding subject must be `celld-system/celld-celld-operator`. The raw fallback file `config/rbac/fleet-namespace.yaml` instead names `celld-system/celld-operator` for the plain-manifest installation; edit its subject if you use that file with Helm. Then check the condition again. This is a scoped permission grant; the operator does not receive blanket delete access across namespaces.

For `ServiceAccountMissing`, create the named runtime ServiceAccount and configure its external AWS identity. For `IsolationUnverified`, independently verify that your CNI enforces NetworkPolicy before enabling the operator's `networkPolicyEnforced` assertion. The assertion does not install a CNI. Check [AWS setup](../../configure/aws/) and [networking](../../configure/networking/) for those prerequisites.

Installation is complete when the manager deployment is available, the CRD exists, the namespace permissions are in place, and the fleet has moved beyond installation blockers. A remaining Pod or PVC problem belongs in [scheduling troubleshooting](../scheduling/).
