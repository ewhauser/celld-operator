# Dedicated-preview density experiment

Date: 2026-09-23. Status: local investigation. These are native arm64
Docker/MinIO observations using the released, unmodified celld
`v0.5.1-ewhauser.3` and the actual operator launcher built from this checkout.
They are not Kubernetes, EKS, AWS S3, R2 or production throughput qualification.

## Results

**100 dedicated runtime/launcher pairs fit comfortably in the local VM for
this small workload.** The corrected 100-fleet run passed 3,200/3,200 health
probes both before and after load, completed 100/100 write/read pairs, and
restored a value after replacing one fleet's entire local filesystem. All 101
stopped containers (including that replacement) exited 0 with no OOM kill.
[Corrected raw run](density-100.json); [calculated summary](summary.json).

| Phase | Fleets | Mean working MiB/fleet | Total working MiB | Total runtime CPU cores |
| --- | ---: | ---: | ---: | ---: |
| baseline-10-idle | 10 | 16.1 | 161 | 0.091 |
| small-30-idle | 30 | 15.6 | 467 | 0.222 |
| small-50-idle | 50 | 16.0 | 799 | 0.150 |
| small-100-idle | 100 | 15.9 | 1595 | 0.226 |
| small-100-one-cell | 100 | 21.0 | 2102 | 0.238 |

The first two rows precede the accounting-proxy connection fix. Short windows,
startup timing and a non-dedicated host make CPU comparisons noisy; the lower
CPU at higher counts is not evidence that adding fleets reduces total cost.
Bounds mostly constrain growth under load; they do not materially reduce the
already-small idle process footprint.

During a 142-second interval at 100 fleets, shared MinIO peaked at 367MiB
working memory and averaged 75 millicores; the accounting proxy peaked at
30MiB and averaged 62 millicores. That interval includes idle and the tiny
one-cell workload, with its initial 30 seconds excluded. MinIO's data directory
was a 512MiB **tmpfs**, so these figures do not represent persistent-disk I/O.
[Shared service samples](shared-services-100.json).

The 100-fleet launch had median health readiness of 12.27 seconds and p95 of
15.09 seconds with cached images. The fresh-filesystem replacement restored its
value after health readiness in 11.47 seconds. Docker reported an average
217,088-byte writable layer per fleet after the one-cell workload; that is a
tiny fixture's layer size, not a justified general scratch-space quota.

The corrected idle run counted 0.709 writes/listings and 0.411 reads per second
per fleet. At the archived S3 rates below, 1,000 always-on fleets project to
**$9,620 per 30 days in background requests alone**. This is why scale-to-zero
or a shared cluster-local storage service matters more than shrinking the
runtime's memory limit. A 1% active fraction would reduce that background term
to roughly $96/month, before wake/deploy/application activity and other costs.

## Method and evidence

- Docker VM: 4 CPUs, 8,307,101,696 bytes RAM (7.74 GiB), Linux arm64.
  Other development containers were running and were not modified.
- Runtime and auxiliary image digests are archived in [images.json](images.json).
  Each run records the launcher SHA-256 and Docker/kernel versions.
- One launcher + one celld per container, one isolated prefix per fleet in a
  shared disposable MinIO bucket. MinIO and the S3 accounting proxy are shared
  infrastructure and excluded from per-fleet memory/CPU figures.
- Baseline uses the operator's runtime settings and a 1GiB container limit.
  Small uses a 128MiB limit and the bounded settings in the
  [runner README](../../../hack/preview-density/README.md). Docker has no
  Kubernetes CPU/memory requests; the tests do not validate a proposed request
  under scheduler contention.
- An initial stateless request loads the application. About every two seconds
  the driver probes the normal health endpoint. Cgroup working memory and CPU
  are sampled during 65-second windows. Working memory excludes inactive file
  cache; it is not process RSS or the full cgroup charge used for OOM decisions.
- The stateful fixture writes/reads 128KiB per object. Every fleet uses the same
  script/class/object names with fleet-specific values. This checks functional
  separation, not credentials/IAM or all binding types.
- The larger fixture creates 32 objects in each of two fleets at each memory
  limit. Within each fleet operations are sequential; up to four fleet clients
  run concurrently. It does not test peak throughput or overload behavior.
- Recovery removes the old container and its entire writable filesystem after
  graceful stop, then launches a new node against the old prefix. It verifies
  one acknowledged value. This is not a crash or infrastructure-loss test, nor
  an implemented operator sleep/wake path.

## Interrupted scale attempts

The original [density.json](density.json) completed the baseline and 10/30/50
fleet measurements. During expansion to 100, the accounting proxy returned a
502 on a conditional write of `fleet/peer-auth.json`; the runtime refused to
continue because that write might have committed. This is a failed startup,
not a successful 100-fleet measurement. The old proxy counted a few transport
errors but did not retain their causes. Socket reuse across MinIO's idle-close
boundary was a suspected harness issue, not a proven runtime defect. The proxy
was changed to avoid upstream socket reuse and to record error codes.

A direct 100-fleet retry was then declined by the conservative memory guard;
[density-guard.json](density-guard.json) records that no fleets were started.
The successful rerun increased in stages again, using actual working-memory
readings to determine that the 100 target fit. A `completed` field means the runner finished
its orchestration; it does not turn a guard stop or failing workload into a pass.

## Memory limits and stateful correctness

[limits.json](limits.json) records 64/64 successful write/read pairs at 128MiB
and another 64/64 at 256MiB. Both fresh-filesystem recovery checks succeeded in
about 10.26 seconds. All six stopped containers exited with code 0 and no OOM.
Sampled memory peaks after load were about 49–50MiB, settling near 32MiB.
The application is deliberately small; these numbers do not establish a safe
universal 128MiB limit. A 64MiB request / 256MiB limit is a reasonable next
**candidate** profile to qualify, with a 25m CPU request and burst capacity.

The earlier [64MiB attempt](64-aborted.json) was deliberately interrupted after
stateful work stalled for more than two minutes. An independent new-object
request timed out after three seconds. Health responses had been successful.
The run was not allowed to finish its workload, so there is no complete pass/
fail count; do not use its lower idle memory as evidence of a viable profile.

The launcher itself sleeps ten seconds before invoking celld. The early
`limits.json` and `density.json` runner recorded the first successful application
response in a field named `readySeconds`; **that field is not Kubernetes
readiness latency**. The runtime serves some requests while its first-readiness
gate is still closed. Logs from the ten-fleet baseline showed a further roughly
four-second gate wait. Early idle windows include startup health 503s; their
counts are retained rather than erased. The runner was corrected for subsequent
runs to wait for both application response and health 200, and records both
`firstResponseSeconds` and `readySeconds`. The early 10.26-second recovery value
is a direct restored read, not an end-to-end URL wake measurement. All timings
exclude Kubernetes scheduling, image pulls, routing updates and an activator.

## Slower background cadence

[quiet.json](quiet.json) tests two small fleets for 185 seconds with a 60-second
lease TTL (normally 10), a 300-second deployment poll (normally 30), and
balancing disabled. Both fleets remained healthy for all 184 probes. After the
measurement, both wrote the same logical object/key and both were read again;
each still returned its own value, checking prefix separation after both writes.

Traffic fell to 0.453 writes/listings and 0.383 reads per second per fleet,
projecting to **$6,268 per 30 days for 1,000 always-on fleets** at the same S3
rates—about 35% below the corrected 100-fleet idle result. This is a small,
separate run, not a controlled 1,000-fleet cloud comparison. Its 185-second
window does not cover a full 300-second deploy poll, so that periodic GET is
absent from the estimate. Adding one GET per 300 seconds is about another
$3.46/month at 1,000 fleets. Fleet bookkeeping and wake scanning remain active
when balancing is off.

The operator owns the ten-second lease TTL today; overriding it requires an
explicit lifecycle-aware API change. A longer TTL also changes crash/fencing
latency and needs qualification. These settings were tested against the
released runtime directly; this is not a supported operator profile or a reason
to bypass the reserved-variable guard. Slower deployment polling can delay
application updates when the push path does not reach a node.

## Storage-cost method

The accounting proxy counts actual S3-compatible methods, key categories and
HTTP statuses during each measurement window. It sees retries and conditional
conflicts. Proxy transport errors are listed separately and excluded from the
price calculation. These are local MinIO requests, not an observed cloud bill.
Post-load windows also include hibernation/cleanup work, so they are not the
same as steady-state empty-fleet windows.

Current AWS US East (N. Virginia) request rates were retrieved from the
[official regional price list](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/us-east-1/index.json)
on the investigation date. The relevant records are preserved in
[s3-pricing.json](s3-pricing.json): $0.005 per 1,000 PUT/COPY/POST/LIST and
$0.0004 per 1,000 GET/other requests. The
[S3 pricing documentation](https://aws.amazon.com/s3/pricing/) confirms LIST
uses the write tier and DELETE is free.

For an illustrative 30-day, always-running population of N fleets:

```text
monthly request charge = N × 2,592,000 ×
    (writes_and_lists_per_fleet_second × 0.000005
     + reads_per_fleet_second × 0.0000004)
```

This linear extrapolation excludes compute, storage bytes, data transfer,
logging, tax, free tiers, discounts, and workload/deploy/wake traffic. It assumes
the measured request cadence persists at scale; it is not AWS qualification.
Changing provider only changes the rates, not the idle traffic source. A
cluster-local object store trades per-request billing for its own infrastructure
and operational cost.

## Limits of the conclusion

There is no measurement of 1,000 fleets, Kubernetes control-plane overhead,
pod/IP capacity, routing controller behavior, wildcard DNS/TLS, scheduler
contention, real application bundles, long-lived connections, alarms/workflow
wakeups, host failure, cloud storage latency or long-duration memory growth.
Sampled memory peaks miss short bursts. Cached images and a local object store
make startup cheaper than a cold production node.

Cgroups reported about 24 tasks (including threads) per runtime/launcher pair
on this four-CPU host. At high density, thread/PID budgets also matter; a
larger-core-count node may have different V8 thread overhead.

Stable fleet reconciliation currently requeues every 5–7 seconds. At 1,000
fleets this alone implies roughly 143–200 reconciles/second before events;
actual API/status operations per reconcile need a cluster measurement. The
runtime-only density experiment does not include this overhead.

## Reproduce

See the [runner and commands](../../../hack/preview-density/README.md). The
scripts create and remove only their own named resources. The 64MiB interrupted
run was explicitly cleaned up. No production configuration, runtime source or
operator reconciliation behavior was modified for the investigation.

See [the resulting design and implementation sequence](../../research/cheap-dedicated-previews.md).
