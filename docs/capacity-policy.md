# Experimental capacity policy

Both Bucket and PersistentFleet support optional shadow recommendations and
explicitly enabled bounded scale-out. All infrastructure and qualification limits
in [fleet API](fleet-api.md) still apply. Manual contraction executes for Bucket
and launcher-managed PersistentFleet fleets. `Automatic` contraction executes only
in the disposable local fixture; against production evidence it reports
`BucketAutomaticUnqualified` or `PersistentAutomaticUnqualified` until the AWS
release gate closes. CPU, memory and readiness cannot certify durability; Pod AZ
spread does not establish follower AZ diversity.

Start with `spec.capacity: {}` or the [shadow example](../config/samples/capacity-shadow.yaml).
The operator reads private celld `/state` and the Kubernetes Metrics Server API.
Install Metrics Server externally and allow operator Pod access to fleet port
8081 using the existing trusted operator labels/namespace. Prometheus is optional.
Missing metrics block policy actions while normal infrastructure reconciliation
continues. The collector never provisions nodes, IAM, S3, EBS drivers or Metrics
Server. Strict configured AZ placement is unchanged.

| Field under `capacity` | Default | Meaning |
| --- | --- | --- |
| mode | Shadow | Shadow, ScaleOut (explicit additions), Automatic (requests both directions) |
| minReplicas / maxReplicas | 3 / 10 | Automatic bounds; minimum must cover the explicit AZ count |
| scaleOutStep | 1 | At most this many additions per completed stable decision; 1–10 |
| sampleIntervalSeconds | 15 | Minimum interval between counted observations |
| maxAgeSeconds | 45 | Maximum source/receipt age and gap between observations |
| minWindowSeconds / maxWindowSeconds | 5 / 60 | Allowed Metrics Server CPU averaging window |
| minSamples | 3 | Distinct advancing observations required in each stable window |
| scaleOutStabilizationSeconds | 30 | Continuous high-demand window |
| scaleInStabilizationSeconds | 600 | Continuous low-demand window |
| scaleOutCooldownSeconds | 300 | Minimum time from last durable action to another addition |
| scaleInCooldownSeconds | 900 | Minimum time from last durable action to a removal request |
| provisioningTimeoutSeconds | 600 | Deadline for useful capacity, after which additions remain blocked |
| redistributionObservationSeconds | 120 | Continuous complete observation window for judging an addition, 30–3600 seconds |
| cpuHighMillicores / cpuLowMillicores | 200 / 80 | Absolute per-container CPU thresholds |
| memoryHighMiB / memoryLowMiB | 768 / 384 | Absolute per-container memory thresholds |

These defaults are unqualified starting points. If the source updates less often
than the configured sampling interval, repeated timestamps restart stabilization;
the interval must accommodate the deployed Metrics Server cadence. CPU is not normalized to resource
requests, and memory uses Metrics Server's container measurement. High demand on
any one node suffices to recommend a bounded addition, but both runtime and
resource observations must cover every expected replica. Contraction requires
all nodes low, fresh and ready plus separately qualified lifecycle evidence.

`status.capacity` reports desired (recommended) count, useful/pending/covered
counts, mode, reason and explanation. Shadow's desired count is informational.
PendingCapacity means requested capacity is not yet observed useful;
IneffectiveCapacity means it exceeded the join/provisioning deadline. Inspect Pod
events/PVCs for scheduling and disk issues and `/state` for pressured incumbents.
A joiner held unready by incumbent pressure does not become useful just because
its CPU is low. IncompleteMetrics and RepeatedSamples hold decisions and reset
stabilization. RateLimited and StabilizingOut/In explain waiting. Lifecycle
conditions separately expose unqualified contractions, drift, disk conflicts,
recovery uncertainty and sticky loss findings.

Only the lifecycle journal writes workloads. `spec.replicas` stays unchanged by
automation. Editing its value overrides the next operation; configure a policy
floor with minReplicas instead of repeatedly editing replicas. A manual override
can exceed policy bounds, subject to the existing manual API constraints. Shadow
pauses new automatic actions at the applied count. Removing the policy returns
to the manual replica target and may request a blocked contraction.

Recorded additions finish their original target after a mode, bound or manual
edit. New actions wait for completion, readiness, cooldown and a new stable
window. A recorded automatic removal freezes if policy/manual intent changes
before issuance; recovery of an already issued action still completes. The
journal survives controller restart, leader change and status clearing. Do not
edit reservation annotations to reset it. See [ADR 0013](decisions/0013-capacity-policy.md)
for field ownership, timestamps and concurrency rules.

After an addition, `ObservingRedistribution` holds another pressure-driven batch
while observing its effect. The journal retains the incumbent container identities
and CPU/memory/pressure baseline. Two consecutive additions with idle newcomers,
unchanged incumbent pressure and no independent CPU increase set
`LoadNotRedistributed`. Ready pods alone do not release this hold. Release requires
the full observation window and minimum sample count showing active newcomers,
fewer pressured incumbents, or aggregate CPU growth exceeding both 10% of the
baseline and `cpuLowMillicores`. Newcomer activity uses the configured low CPU
threshold. Memory footprint or backlog alone is not evidence of useful
redistribution. These are conservative diagnostics, not proof of improved
application latency.

Missing, repeated or stale samples reset the observation window. Intervening
contrary samples invalidate improvement even between counted sample intervals.
Restart, pause and policy edits retain the failed-batch count and baseline;
pause resets positive observation windows. Replaced incumbent identities report
`RedistributionUnknown` rather than treating their disappearance as relief.
An explicit minimum still permits bounded additions to reach that floor, and
manual replica commands retain their existing precedence. Do not edit reservation
annotations to clear holds.

Fleet reconciliations use four workers, so one slow collector does not occupy the
entire controller. Each collection still has its ten-second deadline and eight
per-pod workers. This is bounded concurrency, not an unlimited-fleet timing SLA.

[Validation and remaining limitations](qualification/capacity/README.md).

Maintenance pause freezes new decisions and unissued operations, invalidates
positive demand windows and cached actionability, and leaves issued recovery
running. Resume preserves the original operation target and requires fresh
evidence. Blocked upgrade/restart requests take precedence over new capacity
operations after the current one completes. See [ADR 0014](decisions/0014-coordinated-maintenance.md).

Bucket update: the shared executor now supports manual logical membership contraction and automatic execution in the fixed local qualification environment. Production automatic Bucket requests remain `BucketAutomaticUnqualified` until EKS/S3 release qualification. See [Bucket lifecycle](bucket-scale-in.md) for the full gates and history rules.
