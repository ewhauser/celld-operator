# Fault injection in the disposable kind cluster

19 September 2026 (UTC), run against the unchanged pinned celld v0.5.0 image and
the manager built from this checkout. Command: `make integration-faults`
(`hack/integration/run.py --faults`, scenarios in `hack/integration/faults.py`).
The isolated three-node Kind cluster `celld-step2-07fe1314` ran Calico, MinIO
behind toxiproxy, Metrics Server, the in-cluster manager under its real
ServiceAccount RBAC, and the launcher-managed PersistentFleet path. The cluster
was deleted at the end; [integration.log](integration.log) is the harness
transcript and [reservations.json](reservations.json) the final journals.

These are local safety results, not AWS/EKS/EBS qualification. Every scenario
asserts fail-closed behavior; none certifies liveness after an outage.

## Scenarios and outcomes

| Fault | Injection | Asserted | Result |
| --- | --- | --- | --- |
| Manager crash after replica CAS, before journal | `--local-fault-point=after-effect` on Bucket scale-out 2→3 | Workload already at 3 with the operation annotation while the journal still said 2; restarted manager reconstructs without another write; exactly one workload spec generation increment and one history entry | Pass |
| Manager crash after durable intent, before CAS | `--local-fault-point=before-effect` on Bucket contraction 3→2 | Intent journaled, workload untouched through a 20 s crash loop; recovered manager issues exactly one decrement; write readable | Pass |
| Bucket node loss | `kubectl cordon` plus deletion of the replica on that node | Replacement Pending under strict hostname separation; requested contraction not issued for 45 s (`BucketRecoveryBlocked`); contraction completes after uncordon; write readable | Pass |
| PersistentFleet node loss | cordon plus deletion of `beta-1` | Ordinal recreated Pending; no decrement, template change or claim replacement for 30 s; returns on the original host with the same PVC UID; write readable | Pass |
| S3 latency | toxiproxy `latency` 250 ms ± 50 ms downstream | Grow and shrink complete; write readable | Pass, 70 s for the cycle |
| S3 partition during an issued contraction | toxiproxy `timeout` (hold connections) injected in phase `Recovering` | For 45 s: no completion, no second decrement, no loss finding, same operation ID; runtimes self-fenced and restarted; write readable after the toxic was removed; operation then completed once | Pass |

The partition landed while the operation was in `Recovering`, which is the
"object storage unavailable during handoff" row of the design's failure table.
In this run the operator completed the operation after evidence returned. That
is not guaranteed: a partition that replaces survivor generations can leave the
operation open with a named blocker, and the scenario accepts either outcome as
long as exactly one effect was issued.

## Injection mechanisms

- **Crash points.** `cmd/celld-operator --local-fault-point` names a boundary in
  `applyReplicas`, the single writer of replica changes. The manager exits with
  code 3 there. The flag requires `--local-test` and is ignored otherwise. The
  harness sets it through the Deployment arguments, waits for the previous
  container log to show the injected exit, then withdraws it; the rollout
  replaces the crash-looping pod.
- **Node loss.** `kubectl cordon` followed by pod deletion. Nothing simulates a
  kernel or kubelet failure; the retained volume test relies on local-path PV
  node affinity plus the launcher's host pin.
- **S3 faults.** In faults mode the fixed `minio.celld-test-store.svc:9000`
  endpoint resolves to a toxiproxy pod labeled `app=minio`, which forwards to the
  real server under `minio-backend`. Toxics are driven through the toxiproxy API
  from a curl pod in the store namespace. Runtimes and the manager's evidence
  reader share the proxied path, as they share the real endpoint in production.

## Limits

- One run. Timing figures are observations under a loaded Docker VM.
- The partition self-fences every runtime within a lease TTL; recovery
  measurements after that are Kubernetes restarts, not runtime failover.
- Metrics Server sampling cadence bounds how fast contraction admission can
  pass, so the 420 s scenario budgets are harness choices, not SLOs.
- The Docker VM `fs.inotify.max_user_instances` limit was raised from 128 to
  1024 for worker kubelets to join alongside other local clusters, and restored
  to 128 afterwards. The harness does not change that setting.
- Pinned digest images are imported into the nodes from the local Docker cache
  with `ctr images import --index-name`, so slow registries no longer abort a
  run; images absent from the cache are still pulled with retries.
