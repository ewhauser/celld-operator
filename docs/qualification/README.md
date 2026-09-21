# Qualification

Qualification applies to an exact runtime, launcher/operator revision and test
environment. The strict-control-plane replacement does not inherit qualification
from the removed private-S3 readers, stock-release adapters or journal executor.
Those older reports remain available in Git history.

| Layer | What it checks | Boundary |
| --- | --- | --- |
| `make check` | Typed wire decoding, exact identities, immutable failures, current-operation recovery and unit/fake-client regressions; race detector and lint. | Simulated runtime and Kubernetes effects. |
| `make test-envtest` | Real API admission, reservation/workload compare-and-swap and exact cleanup preconditions. | No workload controllers or CSI driver. |
| `make test-linux` | Real Linux child processes, inherited locks, crashes and restart denial. | A test container is not runtime-image qualification. |
| Opt-in strict-runtime tests | Real fork binary, MinIO, typed HTTP client, launcher and controller proof capture across controller restart. | Empty runtime disk and simulated Kubernetes resources. |
| Disposable Kind integration | Real workload controllers, networking and acknowledged application writes against an explicitly supplied fork artifact. | Local storage and simulated zones are not EKS/EBS. |
| EKS/EBS qualification | Real IAM, S3, CSI finalizers, attachment behavior and physical deletion. | Outstanding until a recorded exact-artifact run passes. |

The [launcher](../launcher-supervision.md#verification) and
[current-operation](../current-operation.md#regression-and-qualification-coverage)
records include opt-in test commands and the September 20 binary evidence.
Do not treat serialized fixtures as live server observations or the empty-disk
handshake as replicated recovery qualification.

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
`.2 → .3` change for both modes, including fresh Pod/storage identities and all
acknowledged values after strict coordinated shutdown.

Use [the EKS test plan](eks-smoke-plan.md) for cloud validation. Keep unsupported
or blocked outcomes visible; never clear authority or force storage finalizers
to make a test pass.

Run the disposable suite with an actual published fork image digest:

```sh
CELLD_RUNTIME_IMAGE=ghcr.io/ewhauser/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29 \
go run ./hack/integration --suite all
```

The command pins the published fork artifact. Individual suites are `lifecycle`, `maintenance`,
`faults` and `external`. A separately supplied `CELLD_UPGRADE_IMAGE` enables the
second-image upgrade scenario; without one it is explicitly not run. Kind uses
a real local hostpath CSI driver for RWOP/Delete behavior, not EBS.
