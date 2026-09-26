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
make integration-upgrade
```

`make integration` runs every suite except `upgrade`, which needs its own
cluster.

The Make targets use the verified fork digest in `hack/runtime-image.txt`
(v0.5.1-ewhauser.6). `CELLD_RUNTIME_IMAGE` or `--runtime-image` selects another
immutable fork digest. There is no upstream or
unpinned fallback. To qualify an already published operator image without
rebuilding the manager, pass
`--operator-image ghcr.io/ewhauser/celld-operator@sha256:...`.

## What the suites qualify

The suites qualify [ADR 0024](../../docs/decisions/0024-persistentfleet-is-a-statefulset.md):
a PersistentFleet is a StatefulSet whose rolling update restarts one member at a
time on its own disk, celld recovers each member, and the operator replaces a
member that cannot come back on its own. The suite runs the operator with
`--member-replacement-delay=5m`, shorter than the ten-minute default, to keep
the node-failure fault within its time budget.

- **Settled.** The harness counts a change as finished only when the fleet
  reports `Provisioned` for its current generation, the workload has exactly the
  expected updated and ready replicas, and no fleet Pod is still terminating. A
  removed member still draining on SIGTERM is not yet gone. A PersistentFleet
  must also have a PDB that allows one disruption. Disks of removed members
  remain.
- **Acknowledged writes.** Each fleet keeps a ledger of acknowledged writes. It
  includes synchronous batches across twelve cells and a continuous writer Pod.
  The writer records only responses that stored the exact requested ID, and it
  runs through every scale, rollout and fault. Each read is retried for up to 60
  seconds, because a cell handed off by a draining member can briefly time out.
  A write that never reads back fails the run.
- **One member down at a time.** During PersistentFleet rollouts, contractions
  and faults, the harness samples the members every two seconds. It fails if
  more than one expected member is missing, unready or terminating. None of the
  scenarios starts a change while another member is already down.

Every suite first checks isolation and provisioning. It creates an Ordered
Bucket fleet (`alpha`) and a PersistentFleet (`beta`), and checks:

- namespace and NetworkPolicy isolation;
- storage-scope conflicts and refusal of a foreign PVC;
- that admission rejects invalid specs;
- that Pods run celld directly, with no launcher and no scheduling gate;
- that `/state.node_log` reports fleet durability, which shows the runtime got
  the operator's configuration;
- that template drift is converged. The StatefulSet may start rolling the
  drifted template first; at most one member rolls, and every disk is kept.

**lifecycle**

- A Bucket Deployment (`gamma`) provisions, scales 2 → 3 → 2 under write load
  and is deleted.
- The Ordered Bucket grows, removes exactly its highest ordinal, reaches one
  member and regrows.
- The PersistentFleet grows 2 → 3 onto a new claim, and a paused fleet does
  not contract.
- It contracts 3 → 2 one member at a time. No claim is deleted, and every
  PVC/PV/CSI identity stays the same.
- After a manager restart, it regrows and reattaches the removed member's disk.
- It contracts 3 → 1 with the manager Pod killed between steps, then regrows
  1 → 3 onto the kept disks.
- Automatic contraction through Metrics Server uses the same one-member step,
  with the fleet idle, and keeps the removed member's disk.

**maintenance**

- A PersistentFleet rolling restart through `restartToken` needs no
  coordinated downtime. A paused fleet holds the restart. Members are replaced
  one at a time, highest ordinal first, and every PVC/PV/CSI identity stays
  the same. After a manager restart the token does not run again.
- A runtime upgrade on retained disks moves a three-member PersistentFleet from
  the legacy v0.5.1-ewhauser.3 digest to the pinned runtime. The legacy digest
  lacks `node_log`; it provisions and rolls like any other. Pass
  `--upgrade-from` or `CELLD_UPGRADE_FROM_IMAGE` to use another source digest,
  or `none` to skip.
- An Ordered Bucket rolling restart runs with a PDB of one.
- Deleting both profiles removes compute, then PVCs and PVs, and keeps the
  bucket reservation.

**faults** disrupts one PersistentFleet member at a time, under continuous
writes, in each of these ways:

- graceful Pod delete;
- `--force --grace-period=0` delete;
- SIGKILL of celld from the node;
- a node drain. The drained member cannot return while its node is cordoned,
  so it stays unready, the PDB allows no further disruption and a second drain
  is refused;
- the manager Pod killed mid-rollout;
- the manager Pod killed mid-contraction;
- a deleted claim (PVC and Pod deleted), which the StatefulSet recreates;
- a lost volume (the PV removed with its finalizers stripped, as after a
  backend disk loss). Kubernetes marks the claim `Lost` and the operator
  replaces the member;
- a node failure: the kubelet of a node holding a member, but not the store or
  the operator, is stopped. The member's celld keeps running there as a zombie.
  After the unreachable-node toleration, its Pod is evicted and stays
  Terminating. The operator force-deletes it and, after the replacement delay,
  replaces the member's disk. Only then does the kubelet return, and the member
  rejoins on a fresh disk.

The suite then takes every member down at once, first by SIGKILL of every
celld and then by deleting every Pod. celld restarts all of them on their own
disks and they recover each other's logs; the one-member invariant does not
apply, but no disk may change and no member Pod may be replaced after SIGKILL.

Each fault must settle without manual repair, with every acknowledged write
readable. A replaced disk comes back with a fresh identity, and every other disk
is kept. A forced Bucket Pod delete loses nothing. The continuous writer and the
probe Pods that read writes back run on the operator's node, which no fault
disturbs.

**external** uses a real HPA and Metrics Server to write the Bucket fleet's
`/scale` subresource. It checks that External mode never fights the HPA, that
a lowered maximum contracts the fleet one member at a time with the ledger
intact, and that manual ownership returns afterwards.


**upgrade** reproduces [#60](https://github.com/ewhauser/celld-operator/issues/60)
on the released v0.0.5 operator, using its vendored manifests in
`testdata/v0.0.5/` and its signed image. It provisions a three-member
PersistentFleet on the runtime v0.0.5 was qualified with and writes a ledger.
Then it deletes one member's Pod, which makes the v0.0.5 launcher permanently
retire that member's disk; the member stays down. The suite then upgrades the
operator as an administrator would, applying this build's CRDs, manager
manifest and fleet Role, and requires:

- the StatefulSet rolls every member onto plain celld, one at a time, on the
  same disks, including the retired one;
- every acknowledged write is readable;
- the v0.0.5 bookkeeping annotation is gone from the storage reservation;
- nothing is edited by hand.

This suite runs the operator with its default replacement delay, so
self-healing does not replace the retired member's disk before the rollout
restarts it.

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
