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
verified strict 3→2→1→0, deletion of every disk, fresh-disk recovery of all 29
acknowledged KV/SQL writes, and 37/37 entries after new writes. Its failed-deadline
case retained storage. This is native/MinIO evidence, separate from Kind and EBS.

Use [the EKS test plan](eks-smoke-plan.md) for cloud validation. Keep unsupported
or blocked outcomes visible; never clear authority or force storage finalizers
to make a test pass.

Run the disposable suite with an actual published fork image digest:

```sh
CELLD_RUNTIME_IMAGE=ghcr.io/ewhauser/celld@sha256:78f74de9b5482a428b69f175cd1901b59cc363f3aa398ffd197ade0c9a6a20af \
go run ./hack/integration --suite all
```

The command pins the published fork artifact. Individual suites are `lifecycle`, `maintenance`,
`faults` and `external`. A separately supplied `CELLD_UPGRADE_IMAGE` enables the
second-image upgrade scenario; without one it is explicitly not run. Kind uses
a real local hostpath CSI driver for RWOP/Delete behavior, not EBS.
