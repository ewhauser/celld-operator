# Kind integration

The Go harness creates a uniquely named three-node Kind cluster and installs
Calico, MinIO with a bounded memory-backed `/data` volume, Metrics Server and the
upstream hostpath CSI driver. It then runs the actual operator, celld processes
and Kubernetes workload controllers. It never uses the default kubeconfig and
removes only its own cluster.

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

The Make targets use the verified fork digest in `hack/runtime-image.txt`. It
must report `/state.node_log` (v0.5.1-ewhauser.5 or later), because
PersistentFleet settlement is read from it. `CELLD_RUNTIME_IMAGE` or
`--runtime-image` selects another immutable fork digest. There is no upstream or
unpinned fallback. To qualify an already published operator image without
rebuilding the manager, pass
`--operator-image ghcr.io/ewhauser/celld-operator@sha256:...`.

## What the suites qualify

The suites qualify [ADR 0023](../../docs/decisions/0023-node-loss-is-routine.md):
celld tolerates the loss of one member, so the operator disrupts one member at a
time and waits until celld's node-log state says the fleet has absorbed it.

- **Settled.** The harness counts a change as finished only when the fleet
  reports `Provisioned` for its current generation, the workload has exactly the
  expected updated and ready replicas, and no fleet Pod is still terminating. A
  removed member still draining on SIGTERM is not yet gone. A PersistentFleet
  must also keep no disk above its replica count and have a PDB that allows one
  disruption.
- **Acknowledged writes.** Each fleet keeps a ledger of acknowledged writes. It
  includes synchronous batches across twelve cells and a continuous writer Pod.
  The writer records only responses that stored the exact requested ID, and it
  runs through every scale, rollout and fault. Each read is retried for up to 60
  seconds, because a cell handed off by a draining member can briefly time out.
  A write that never reads back fails the run.
- **One disruption at a time.** During PersistentFleet rollouts, contractions
  and faults, the harness samples the members every two seconds. It fails if
  more than one expected member is missing, unready or terminating.

Every suite first checks isolation and provisioning. It creates an Ordered
Bucket fleet (`alpha`) and a PersistentFleet (`beta`), and checks:

- namespace and NetworkPolicy isolation;
- storage-scope conflicts and refusal of a foreign PVC;
- that admission rejects invalid specs;
- that Pods run celld directly, with no launcher and no scheduling gate;
- that `/state.node_log` is present;
- that template drift is converged without replacing members.

**lifecycle**

- A Bucket Deployment (`gamma`) provisions, scales 2 → 3 → 2 under write load
  and is deleted.
- The Ordered Bucket grows, removes exactly its highest ordinal, reaches one
  member and regrows.
- The PersistentFleet grows 2 → 3 onto a fresh claim, and a paused fleet does
  not contract.
- It contracts 3 → 2. The removed member's PVC may be deleted only after its Pod
  is gone and the operator has released it. Surviving disks keep their
  identities, and the old PV and attachment disappear.
- After a manager restart, it regrows onto a new disk identity.
- It contracts 3 → 1 with the manager Pod killed between steps, then regrows
  1 → 3 onto fresh claims.
- Automatic contraction through Metrics Server uses the same one-member step.

**maintenance**

- A PersistentFleet rolling restart through `restartToken` needs no
  coordinated downtime. A paused fleet holds the restart. Members are replaced
  one at a time, highest ordinal first, and every PVC/PV/CSI identity stays
  the same. After a manager restart the token does not run again.
- A runtime upgrade on retained disks moves a three-member PersistentFleet from
  the legacy v0.5.1-ewhauser.4 digest to the pinned runtime. The legacy digest
  lacks `node_log`, so the operator rolls it on readiness plus stabilization,
  and it never reports `Provisioned`. Pass `--upgrade-from` or
  `CELLD_UPGRADE_FROM_IMAGE` to use another source digest, or `none` to skip.
- An Ordered Bucket rolling restart runs with a PDB of one.
- Deleting both profiles removes compute, then PVCs and PVs, and keeps the
  bucket reservation.

**faults** disrupts one PersistentFleet member at a time, under continuous
writes, in each of these ways:

- graceful Pod delete;
- `--force --grace-period=0` delete;
- SIGKILL of celld from the node;
- a node drain. The drained member cannot return while its node is cordoned, so
  the operator lowers the PDB to zero and a second drain is refused;
- the manager Pod killed mid-rollout;
- the manager Pod killed mid-contraction;
- a lost claim (PVC and Pod deleted);
- a lost volume (the PV removed with its finalizers stripped, as after a
  backend disk loss);
- the `celld.eric.dev/replace-member` annotation. An invalid value is reported
  as `ReplaceMemberInvalid`, and a valid one is cleared after use.

Each fault must settle without manual repair, with every acknowledged write
readable. A lost or replaced disk comes back with a fresh identity, and every
other disk is kept. A forced Bucket Pod delete loses nothing.

**external** uses a real HPA and Metrics Server to write the Bucket fleet's
`/scale` subresource. It checks that External mode never fights the HPA, that
a lowered maximum contracts the fleet one member at a time with the ledger
intact, and that manual ownership returns afterwards.

## Limits

These suites establish local Kubernetes, hostpath CSI and celld behavior on
three nodes of one zone. MinIO data is not durable across replacement of its
Pod, and no scenario replaces the store. The suites do not qualify:

- five-member fleets or zone loss;
- the ADR's "last complete copy of a dead session" loss scenario, which needs a
  leader and its only follower to lose their disks together;
- a node-group upgrade running in parallel with a rolling upgrade;
- Istio, IRSA or Datadog admission;
- AWS EBS deletion, EC2 node loss, prolonged S3 partitions or managed service
  behavior.

For the single-node operator-backed preview CLI suite, including multi-object
state cloning, see [local preview integration](../../docs/preview-integration.md)
and run `make integration-previews` with a preview-capable celld image.
