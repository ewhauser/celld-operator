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
make integration-extended
```

The Make targets use the verified fork digest in `hack/runtime-image.txt`. It
must be v0.5.1-ewhauser.6 or later: PersistentFleet settlement is read from
`/state.node_log`, and idle contraction needs `.6`'s follower-loss detection. `CELLD_RUNTIME_IMAGE` or
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
  The fleet is idle during this step, so an idle leader must still move its
  ensemble off the removed member before that member's disk can be released.
  v0.5.1-ewhauser.5 does not do this and fails the step; `.6` does.

**maintenance**

- A PersistentFleet rolling restart through `restartToken` needs no
  coordinated downtime. A paused fleet holds the restart. Members are replaced
  one at a time, highest ordinal first, and every PVC/PV/CSI identity stays
  the same. After a manager restart the token does not run again.
- A runtime upgrade on retained disks moves a three-member PersistentFleet from
  the legacy v0.5.1-ewhauser.3 digest to the pinned runtime. The legacy digest
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

**extended** covers what the other suites' fixtures cannot. It is heavier, so
CI runs it only nightly, on dispatch and on pull requests labeled
`integration`; `--suite all` does not include it. `CELLD_EXTENDED_SCENARIOS`
(`five,loss,drain,admission`) runs a subset locally.

- **Five members.** A five-member PersistentFleet with `placement.mode:
  Relaxed` (preferred host separation, so five members fit on three nodes)
  rolls through `restartToken` one member at a time, highest ordinal first, on
  the same disks. It then contracts 5 → 4 → 3, releasing each removed disk only
  after its member is gone, and regrows to five onto fresh claims. Writes run
  throughout and the ledger is read after every step. After settling, every
  member's `/state.node_log` reports fleet posture and an own ensemble of two
  followers.
- **Last copy of a session.** A two-member PersistentFleet, whose leaders each
  have the other member as their only follower, loses both disks at once:
  celld is SIGKILLed, both PVs are removed without finalizers and both Pods
  are force-deleted right after the writer stops. The operator must replace
  both members without waiting (a `MemberDiskLost` Warning event) and the
  fleet must settle within 15 minutes. Acknowledged writes may be lost here,
  but never silently: if any does not read back within a shared 90-second
  window, the bucket must hold at least one celld loss record
  (`log/<session>.e<epoch>.loss.json`). The counts are printed either way. New
  writes afterwards must all read back.
- **Drain during a rolling restart.** While a three-member PersistentFleet
  rolls, `kubectl drain` targets the node of `beta-0`, which is not yet
  updated. The PDB (zero while a member is down) must refuse the eviction at
  least once and admit it only when the fleet has settled, and no sample may
  show two members down. Strict host separation keeps the drained member off
  the other nodes, so the harness holds for 20 seconds with it down and the
  PDB closed, uncordons, and requires the rollout to finish: every member on
  the update revision, every PVC/PV/CSI identity unchanged, the ledger intact.
- **Admission mutations.** A mutating webhook (`admission/`, built from
  source onto the operator's digest-pinned distroless base, loaded into kind,
  never pulled) serves TLS from a CA the harness generates. It fails closed
  and is scoped to the admission fleets' Pod names. On Pod creation it injects
  what Istio, EKS IRSA and the Datadog admission controller inject: an
  `istio-init` init container, an `istio-proxy` sidecar with its own readiness
  probe, Istio labels, annotations and volumes, `AWS_ROLE_ARN` and
  `AWS_WEB_IDENTITY_TOKEN_FILE` with a projected `sts.amazonaws.com`
  service-account token mounted into every container, and `DD_AGENT_HOST`
  (host IP), `DD_ENTITY_ID` and `DD_ENV` on every container. The proxy
  intercepts no traffic. A three-member PersistentFleet and a three-member
  Ordered Bucket start on the `--upgrade-from` runtime. Under continuous writes
  they upgrade to the pinned runtime, roll through `restartToken` and scale
  3 → 2 → 3, and every Pod is checked for the injected shapes. Their Ready
  reason is sampled throughout and may only ever be `Provisioning`,
  `LifecycleProgress` or `Provisioned`.

## Limits

These suites establish local Kubernetes, hostpath CSI and celld behavior on
three nodes of one zone. MinIO data is not durable across replacement of its
Pod, and no scenario replaces the store. The suites do not qualify:

- zone loss, or a five-member fleet under strict host separation (three nodes
  only fit five members with Relaxed placement);
- a whole node-group upgrade (one drain during one rolling restart is
  covered, not a sequence of node replacements);
- real Istio: traffic interception, mTLS, probe rewriting and mesh routing;
  a real IRSA credential exchange (the fleets still hold MinIO's static test
  credentials); a real Datadog agent;
- AWS EBS deletion, EC2 node loss, prolonged S3 partitions or managed service
  behavior.

For the single-node operator-backed preview CLI suite, including multi-object
state cloning, see [local preview integration](../../docs/preview-integration.md)
and run `make integration-previews` with a preview-capable celld image.
