# Strict control-plane integration

The Go harness creates a uniquely named three-node Kind cluster, installs Calico,
MinIO with a bounded memory-backed `/data` volume, Metrics Server and the upstream hostpath CSI driver, and runs the actual
operator, launcher, celld processes and Kubernetes workload controllers. It never
uses the default kubeconfig and removes only its own cluster and launcher image.

Three kubelets need at least 1,024 inotify instances and 1,048,576 watches in the
Docker Linux host/VM. CI sets these limits explicitly. On a local Docker Desktop
VM, an administrator can raise them using a privileged container before the run;
a low limit causes kubelet startup to fail with `inotify_init: too many open files`.

```
make integration
make integration-lifecycle
make integration-maintenance
make integration-faults
make integration-external
```

The Make targets use the verified fork digest in `hack/runtime-image.txt`.
`CELLD_RUNTIME_IMAGE` or `--runtime-image` can select another immutable fork digest.
Every fleet uses the fork explicitly. There is no upstream or unpinned fallback.
To qualify an already published operator image without rebuilding either Go
binary, pass `--operator-image ghcr.io/ewhauser/celld-operator@sha256:...`.

The lifecycle suite tests both Ordered Bucket and PersistentFleet: namespace and
network isolation, exact-ordinal removal, pause before issue, repeated growth and
contraction including 2 to 1, controller restart, fresh PVC/PV/CSI identities,
automatic contraction using Metrics Server and twelve acknowledged writes per
fleet. Persistent volumes use ReadWriteOncePod, a Delete/WFFC StorageClass and the
CSI provisioner's deletion finalizer. The StatefulSet retains claims until the
operator explicitly deletes them after strict celld completion and process
exclusion. The harness requires old PV and VolumeAttachment disappearance before
calling the operation complete.

Maintenance tests coordinated downtime, restart recovery across manager
replacement, fresh disks, token replay protection, final deletion and permanent
bucket reservations. Set `CELLD_UPGRADE_IMAGE` to a different strict-compatible
fork digest to test a runtime image transition and the acknowledged-write ledger.
Without that second digest the harness explicitly reports the upgrade as not run.
The operator's unit tests independently cover the upgrade state machine.

Fault tests terminate the actual manager before and after a guarded workload
update, then require the same operation to complete with exactly one replica
write. They also run contraction through injected S3 latency and preserve fleet
counts through a short storage outage. The external suite uses a real HPA and
Metrics Server to write the fleet's `/scale` subresource.

These suites establish local Kubernetes, protocol and hostpath CSI behavior.
MinIO data is not durable across replacement of its Pod; these scenarios do not
replace the store. They do not qualify AWS EBS deletion, EC2 node loss, prolonged S3 partitions or
managed service behavior. Unknown or lost strict completion deliberately blocks
removal; the harness does not repair that ambiguity by deleting disks.
