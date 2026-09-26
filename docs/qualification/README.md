# Test evidence

Test evidence applies to an exact runtime, operator revision and test
environment. The current PersistentFleet lifecycle
([ADR 0024](../decisions/0024-persistentfleet-is-a-statefulset.md)) does not
inherit results from earlier designs. The dated records below cover earlier
releases. Reports for the removed private-S3 readers, stock-release adapters
and journal executor remain available in Git history.

| Layer | What it checks | Boundary |
| --- | --- | --- |
| `make check` | Typed wire decoding, exact identities, immutable failures, rendered workloads, rollout and contraction steps, self-healing actions and the cases they leave alone, adoption of earlier fleet state and unit/fake-client regressions; race detector and lint. | Simulated runtime and Kubernetes effects. |
| `make test-envtest` | Real API admission, reservation/workload compare-and-swap and exact cleanup preconditions. | No workload controllers or CSI driver. |
| `make test-linux` | The controller tests on Linux, inside a container of the pinned runtime image. | A test container is not runtime-image qualification. |
| Disposable Kind integration | Real workload controllers, networking, hostpath CSI and acknowledged application writes against an explicitly supplied fork artifact. | Local storage and simulated zones are not EKS/EBS. |
| AWS/EBS deployment | User reports v0.0.2 deployed with PersistentFleet and EBS volumes successfully. | The deployment has no archived failure or deletion test receipt here. |

## Current Kind coverage

The Kind suites run the real operator and fork under continuous write load and
require every acknowledged write to be readable afterwards. The operator runs
with `--member-replacement-delay=5m`. For PersistentFleet the suites exercise:

- a rolling restart and a runtime upgrade on retained disks;
- a graceful Pod delete, a forced Pod delete, `SIGKILL` of celld, a node drain
  held by the budget, and an operator restart in the middle of a rollout and of
  a contraction;
- a deleted claim, which the StatefulSet recreates;
- a lost volume, which the operator replaces with no manual step;
- a node failure: the kubelet stops while celld keeps running on that node.
  The operator force-deletes the stranded Pod and, after the replacement
  delay, replaces the member's disk;
- scale-in that keeps the removed member's disk, and growth that reattaches it;
- fleet deletion that removes compute, then disks, and keeps the bucket
  reservation.

This page does not yet record a run of these suites against the current
lifecycle. Hostpath CSI has no attach operation; use the AWS deployment's CSI
events and EBS records to verify detach and deletion.

Run the disposable suite with an actual published fork image digest:

```sh
CELLD_RUNTIME_IMAGE=ghcr.io/ewhauser/celld@sha256:07a81e72155890b36529756a3ecbb22045d94679b3001a0341cacc044337549a \
go run ./hack/integration --suite all
```

The command pins the published `.6` fork artifact, the same digest as
`hack/runtime-image.txt`, which `make integration` uses. Individual suites are
`lifecycle`, `maintenance`, `faults` and `external`. The maintenance suite
upgrades a PersistentFleet from the earlier `.3` digest on retained disks;
`--upgrade-from` or `CELLD_UPGRADE_FROM_IMAGE` selects another source digest,
and `none` skips it. Kind uses a real local hostpath CSI driver for RWOP/Delete
behavior, not EBS.

## Earlier evidence

The strict-control-plane implementation's
[four-suite Kind matrix](https://github.com/ewhauser/celld-operator/actions/runs/35550557670),
[standard CI](https://github.com/ewhauser/celld-operator/actions/runs/35550557823)
and [site build](https://github.com/ewhauser/celld-operator/actions/runs/35550557853)
passed at `a893b9b` with the published `.3` runtime. The
[delayed-witness report](native-peer-startup/README.md) contains native `.3` and
hosted evidence: 10/10 native and 24/24 Kind writes per fleet after recovery.
Native full-stop cases recover 28/28, 20/20 and 21/21 writes through every fresh
node, and failed-deadline checks retain storage.

The [strict control-plane run](strict-control-plane/README.md) records the
September 20 Kind run of the operator and launcher. The launcher's opt-in test
commands and September 20 binary evidence are in Git history. Do not treat
serialized fixtures as live server observations or the empty-disk handshake as
replicated recovery qualification.

The [released native maintenance run](native-maintenance/README.md) separately
verified populated sequential and concurrent full stops with the released `.2`
binary. After deletion of every original disk, all three fresh nodes recovered
28/28, 20/20 and 21/21 acknowledged writes across the three schedules, including
a peer-only acknowledgment. Its failed-deadline case retained storage. This is
native/MinIO evidence, separate from Kind and EBS.

The [native peer-address comparison](native-peer-addresses/README.md) reproduces
acknowledged-tail loss after changed peer endpoints and verifies 10/10 recovery
with stable endpoints. It motivated PersistentFleet ordinal DNS. The later
[delayed-witness qualification](native-peer-startup/README.md) reproduced a
separate startup-skew defect in `.2` and tracks its fail-closed correction.

The [published runtime upgrade](runtime-upgrade/README.md) verifies the actual
`.2 → .3` change for both modes: 12/12 acknowledged values in each profile after
strict coordinated shutdown, with fresh Pod and storage identities.

Use [the EKS test plan](eks-smoke-plan.md) when exercising additional cloud
failure scenarios. Its strict-shutdown, proof-capture, coordinated-downtime and
launcher steps predate ADR 0024. Keep unsupported or blocked outcomes visible;
never clear authority or force storage finalizers to make a test pass.
