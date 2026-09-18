# Bucket preflight validation

18 September 2026, against parent commit `119993c` plus the uncommitted Bucket
preflight changes. This is **not completed operator scale-in qualification**.

Passed:

- `make check`: build, race-enabled tests and lint (zero issues).
- `make manifests-check`: generated API/CRDs unchanged and consistent.
- `make qualification-replay`: five positive peer-recovery fixtures and four
  expected blocks, including absent Bucket logs.
- `make qualification-test`: eight harness tests.
- `make qualification-local SCENARIO=bucket OUTPUT=.qualification-runs/bucket-preflight-20260918`:
  fresh dedicated Docker/MinIO scenario, exported by the existing redacting
  exporter. Harness cleanup completed normally. No AWS or kubeconfig used.

New Go fault tests distinguish a no-peer-log observation from peer-recovery
completion; they reject missing/denied/partial metadata, late loss declarations,
stale observations, historical or replaced generations, invalid candidate
ownership/configuration and candidate changes during reads. Production reason
checks prove successful preflight does not bypass session binding or completion
gates. Existing journal crash/cancellation and manual/automatic fail-closed tests
also passed. No new executor exists, so repeated successful operator contractions
or its post-removal settling cannot be claimed as tested.

The fresh unchanged-runtime scenario retained 70/70 acknowledged operation IDs
across graceful termination (0.201 seconds, exit 0), abrupt process loss
(0.151 seconds, exit 137), restart and deadline-cut storage stall
(0.144 seconds, exit 0). No matching sealed peer-log record appeared during the
roughly 46.5-second observation windows. The old generic recovery assessment
correctly remained blocked; this is not evidence that those writes were lost.
The ledger supports the pinned Bucket durability argument for this small local
workload, not dormant-cell completeness, partitioned-host fencing, EKS/S3, a
performance bound or a weaker adopted operation-completion contract.

See [the implementation boundary and remaining tasks](../../bucket-scale-in.md).
