# Experimental capacity policy

Both Bucket and PersistentFleet support optional shadow recommendations and
explicitly enabled bounded scale-out. All infrastructure and qualification limits
in [fleet API](fleet-api.md) still apply. **The installed manager cannot execute
scale-in in either profile.** `Automatic` reports requests through the exact
BucketCompletionUnqualified/FencingUnqualified gates from step 3. CPU, memory and
readiness cannot certify durability; Pod AZ spread does not establish follower
AZ diversity.

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

[Validation and remaining limitations](qualification/capacity/README.md).

Maintenance pause freezes new decisions and unissued operations, invalidates
positive demand windows and cached actionability, and leaves issued recovery
running. Resume preserves the original operation target and requires fresh
evidence. Blocked upgrade/restart requests take precedence over new capacity
operations after the current one completes. See [ADR 0014](decisions/0014-coordinated-maintenance.md).
