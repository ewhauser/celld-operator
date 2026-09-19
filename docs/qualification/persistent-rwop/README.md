# PersistentFleet on ReadWriteOncePod CSI volumes (kind)

19 September 2026. `make integration-persistent-rwop` runs the PersistentFleet
lifecycle suite with claims requested as `ReadWriteOncePod` and served by the
kubernetes-csi hostpath driver v1.18.0 in its per-node ("distributed")
deployment, instead of the local-path `ReadWriteOnce` exception. The manager
runs with `--local-test --local-rwop`; the driver, its provisioner RBAC and
CSIDriver object are applied from SHA-pinned upstream manifests, and the
`retained` StorageClass uses `hostpath.csi.k8s.io` with `Retain` and
`WaitForFirstConsumer`.

Result of the first run (cluster `celld-step2-867875b6` in [integration.log](integration.log)):
all 28 stages passed, including

- both PersistentFleet claims bound as `ReadWriteOncePod` to CSI volumes with
  driver `hostpath.csi.k8s.io`;
- two manual 3→2→3 cycles with graceful retirement, controller restart,
  unchanged PVC UIDs and same-host reactivation;
- live automatic 3→2 contraction with real Metrics Server data;
- the acknowledged application write readable throughout.

## What this adds and what it does not

It closes the gap where the shipped access mode was never exercised outside
unit tests: the operator now provisions, retires and reactivates real CSI
volumes with `ReadWriteOncePod` semantics on a real scheduler. The hostpath
driver is node-local, so it also exercises topology-constrained rebinding.

It is not EBS or attachment qualification. The hostpath driver has
`attachRequired: false`, so `VolumeAttachment` evidence and the EBS-specific
identity checks stay on the EKS path (see the [EKS smoke plan](../eks-smoke-plan.md)).
Cross-node handoff remains blocked in local mode by design.
