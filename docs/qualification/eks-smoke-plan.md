# EKS smoke suite plan

Status: **not implemented and not run.** No AWS account, cluster, bucket or role
has been supplied to this repository. This page defines the smallest cloud
suite that covers what the local kind and Docker fixtures cannot, so it can be
built and run as soon as an explicitly named nonproduction account exists.
Nothing below is evidence.

## Why a separate suite

The kind suites (`make integration*`) and the fault-injection suite prove
controller safety against a real API server, real scheduling, MinIO and Metrics
Server. They cannot exercise:

- EBS CSI attach, detach and ReadWriteOncePod enforcement across real nodes.
- IAM denial of the operator's read-only identity for writes and bundle bodies.
- EKS Pod Identity or IRSA token exchange from the runtime and the operator.
- S3 pagination, conditional writes and consistency under real network faults.
- Node termination as an infrastructure event rather than `kubectl cordon`.

## Prerequisites (all supplied externally, none created by the suite)

- A named nonproduction AWS account and region, an EKS cluster with at least
  three worker nodes across the requested AZ count, a policy-enforcing CNI, the
  EBS CSI driver, a `Retain`/`WaitForFirstConsumer` gp3 StorageClass, and
  Metrics Server.
- One dedicated S3 bucket per fleet with no lifecycle expiry on `nodes/` or
  `log/`.
- Two identities: the runtime role with bucket read/write, and a separate
  operator role limited to `GetObject` on `nodes/*` and `ListBucket` on the
  `nodes/` and `log/` prefixes. Both wired through Pod Identity or IRSA.
- An explicit kubeconfig context passed on the command line. The suite must
  refuse to run against a default context, exactly as the kind harness does.
- Written permission for the fault injections below on that account.

## Checks, in order

| # | Check | Passes when | Local substitute |
| --- | --- | --- | --- |
| 1 | Operator identity denial | A `PutObject`, `DeleteObject` and a `GetObject` on `log/…` bundle body from the operator role are all denied; `nodes/` reads and prefix lists succeed | None |
| 2 | Runtime identity | Both profiles reach Ready with Pod Identity/IRSA only; no static credentials anywhere in the pod spec | MinIO static credentials |
| 3 | RWOP enforcement | A second pod mounting a fleet PVC is refused by the scheduler; the launcher's lock is never reached | hostpath CSI (`make integration-persistent-rwop`) |
| 4 | Retained EBS reattach | PersistentFleet 3→2→3 keeps the PVC and PV UIDs and the EBS volume ID; the reattached ordinal starts on a node in the volume's AZ | local-path/hostpath, same host only |
| 5 | Cross-AZ reuse blocked | Cordoning every node in the volume's AZ leaves the ordinal Pending with a named condition; nothing recreates the disk | None |
| 6 | S3 pagination | A fleet whose `log/` prefix exceeds one listing page (seed >1000 objects) still passes inventory; a fleet exceeding the operator's key budget blocks visibly | MinIO small listings |
| 7 | Node termination | Terminate the EC2 instance under one Bucket replica during steady state; contraction stays blocked until membership converges; acknowledged writes remain readable | `kubectl cordon` + pod delete |
| 8 | Partition | Deny the runtime security group egress to S3 for 60 s during an issued contraction; same invariants as the kind partition scenario | toxiproxy |
| 9 | Opt-in EC2 fencing | With `--ec2-fencing-*` set, an unreachable admitted donor is terminated exactly once and the contraction completes with a positive `terminated` receipt | Fake infrastructure API in unit tests |

Every check records the reservation journal before and after, the acknowledged
write ledger, and the exact IAM policy documents used.

## Shape of the harness

`hack/eks/smoke.py`, mirroring `hack/integration/run.py`: explicit `--context`,
`--namespace`, `--bucket-prefix`, `--operator-role-arn`, `--runtime-role-arn`
flags with no defaults and no fallback to the AWS SDK default chain for
discovery; every created Kubernetes object carries a run label; cleanup deletes
only labeled objects and never touches buckets, volumes or IAM. Fleet CRs use
`config/samples/*.yaml` with names, buckets and AZs substituted. Fault steps
that call EC2 or security-group APIs are gated behind an `--allow-faults` flag
and print the exact call before making it.

## What passing would and would not mean

Passing every row establishes that the operator's cloud integrations behave as
the local fixtures assume. It does not establish sustained load behavior,
follower AZ diversity, or multi-day soak, which the qualification index lists
separately.
