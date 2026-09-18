# Bucket logical membership qualification

This is local evidence for the unchanged pinned celld v0.5.0 image and the
experimental Bucket removal executor. It does not qualify AWS EKS/S3 or
PersistentFleet peer-log recovery.

## Actual runtime fault experiment

Command: `python3 hack/qualification/bucket_fencing.py --output .qualification-runs/bucket-fencing-final`. The exported machine-readable artifacts in this
folder came from `.qualification-runs/bucket-fencing-final`.

- A paused writer remained a running process while its lease expired and a
  survivor acquired the cell. Earlier acknowledged writes remained readable.
- Resuming the old process triggered its self-fencing exit. An in-flight delayed
  response timed out: its write is explicitly ambiguous, not declared absent.
- A separate peer-network partition retained working S3 connectivity. The old
  owner stayed alive, renewed its lease, and successfully acknowledged five more
  writes. That live lease must block logical removal completion.
- After reconnecting, all **36 acknowledged operation IDs** were readable with
  **36 distinct sequence numbers**, with no conflicting successful sequence
  assignment. This finite run supports the pinned source argument; it is not a
  proof for every possible schedule.

`results.json`, `acknowledged-ledger.json`, runtime `/state` captures and node
metadata preserve the evidence. `provenance.json` records the export checksums.
The tested source SHA-256 values are:

- `hack/qualification/bucket_fencing.py`: `b2efab23597f8d6413e296208b97aadc5d254bb81a0c475579d27039714c2a5a`
- `hack/qualification/app/index.js`: `dbdb4455edd653972beeba2012903a1e4b5c5534b0b5b26994f92d7a0800f4d2`

The fixture counter uses actual runtime transactions and S3; it does not fabricate
recovery certificates. The operator still has no access to cell bodies or peer
secrets in production.

## Reproducible controller qualification

- `make check`: build, race-enabled Go tests, golangci-lint.
- `make manifests-check qualification-replay qualification-test`: generated
  manifest consistency, positive/negative adapter replays and Python harness
  tests.
- `make integration-bucket`: isolated three-node Kind, pinned MinIO and real
  Metrics Server; exact manager RBAC; actual pinned runtime Pods. Includes manual
  repeated shrink/grow, pause, manager restart, an unchanged PersistentFleet
  safety block, and automatic contraction using real CPU/memory samples.

Final run on 18 September 2026: all commands above passed. `check.txt` and
`qualification.txt` retain their outputs. `integration.txt` records the complete
successful run and final reservation journals: manual 3→1, regrowth, a paused
request followed by manager restart/removal, another regrowth, then automatic
3→1 using real Metrics Server. The acknowledged application write remained
readable, and five retired sessions remained recorded with unknown physical
liveness. The owned Kind cluster was deleted; the Docker VM inotify limit,
temporarily raised from 128 to 1024 because shared workloads exhausted it, was
restored to 128.

The integration fixture uses one AZ and strict hostname separation. Unit tests
exercise multi-AZ admission for every possible Deployment victim. In particular,
2/1 Pods across two AZs cannot shrink safely without controlling the deletion
victim; 2/2 can shrink once. Relaxed placement remains an explicit user choice.

A live integration attempt exposed runtime garbage collection of completed
historical node records. The executor now permits absent records only after their
positive expiry and settling were durably committed. A new missing record,
unreadable listed record, renewed lease or changed generation still blocks.
Cancellation retains admission without granting completed-retirement authority.

## Remaining release gates

Production automatic contraction remains `BucketAutomaticUnqualified`. Manual
contraction is experimental. AWS S3 consistency/conditional-write behavior under
EKS network and node faults, EKS admission/identity injection, real cloud clock
behavior, sustained traffic and long-lived connections still require release
qualification. No AWS resources or implicit external kubeconfig were used.

Completion reports Kubernetes membership and recovery obligations; it does not
certify that every historical process is physically dead. Lease timestamps rely
on the same bounded-clock assumptions as the pinned runtime. A writer retaining
S3 access and a live lease keeps completion pending.
