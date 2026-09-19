# Implementation dependency validation

Local validation on 2026-09-18, based on `a4030c8` plus the uncommitted dependency
implementation changes:

- `make check`: build, complete Go race suite, and golangci-lint; passed, zero lint issues.
- `make manifests-check`: generated CRDs/deepcopy match; passed.
- `make chart-check`: canonical CRD/RBAC parity, strict Helm lint, rendered chart
  checks including EC2 fencing opt-in, and four release guard tests; passed.
- Eight Python qualification harness tests passed; saved runtime experiment
  harness hashes match the checked-in files.
- Regression coverage includes migration deletion/pause/crash boundaries,
  zero-replica creation before UID-bound activation, sticky loss, interrupted
  settling, coordinated stop/restart, late epoch high-water retention, exact EC2
  termination evidence and durable intent, and runtime image-CAS replay.

These tests do not qualify real EKS, EC2 termination, EBS attachment handoff, or
the full Kubernetes upgrade/migration executor. The separate released-runtime
Docker/MinIO experiment covers storage compatibility for the documented stopped
v0.4.1 to v0.5.0 direction only. No cloud changes or publication were performed.
