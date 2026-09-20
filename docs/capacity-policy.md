# Capacity policy

The built-in collector reads celld's typed `/state` and Kubernetes Metrics Server.
Incomplete observations are invalid, never zero demand. CPU, memory and readiness
do not establish removal safety. Manual and policy requests share the
[bounded current-operation executor](current-operation.md).

| Mode | Replica ownership |
| --- | --- |
| Omitted | Manual `spec.replicas`. |
| Shadow | Observe and report recommendations only. |
| ScaleOut | Apply stable bounded additions. |
| Automatic | Request additions and contractions through the strict executor. |
| External | One external `/scale` writer owns `spec.replicas`; no built-in demand collection. |

StatefulSet contraction removes one highest ordinal after exact strict proof.
Bucket Deployment contraction is blocked. These paths remain experimental;
[cloud qualification](qualification/README.md) is separate from execution.

| Field under `capacity` | Default | Meaning |
| --- | --- | --- |
| mode | Shadow | Shadow, ScaleOut (explicit additions), Automatic (requests both directions), External (one `/scale` writer owns `spec.replicas`) |
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


These defaults are starting points, not production capacity guarantees. Source
samples must advance and fit configured age/window limits. Built-in contraction
requires complete fresh low-demand observations and ready survivors; it still
requires strict shutdown and storage proof from the executor.

`spec.replicas` is never rewritten by built-in automation. A manual edit takes
precedence for the next operation. Policy mode and bounds do not retarget issued
work. Removing a policy returns toward the manual target. Shadow pauses automatic
actions at the applied count.

Current capacity state retains stabilization timestamps, cooldown and bounded
redistribution observations. `PendingCapacity` and `IneffectiveCapacity` identify
additions not yet useful. `IncompleteMetrics` and `RepeatedSamples` reset stable
windows. `ObservingRedistribution` waits for newcomer activity or measured relief;
`LoadNotRedistributed` holds repeated ineffective additions. An idle ready Pod
alone is not evidence of useful redistribution. Explicit minimum capacity and
manual replica requests retain their documented precedence.

Maintenance pause stops new/unissued work and invalidates positive demand windows.
After Requesting, the recorded operation keeps its identity and deadline until
completion or visible blockage. Controller restart or status clearing does not
reset authority. Never edit reservation annotations to remove a policy hold.

External autoscalers target `CelldFleet` using its `/scale` subresource, never the
managed workload. The subresource reports nonterminal observed Pods, including
terminating ones, with the fleet's exact label selector. A desired target that
cannot pass lifecycle checks remains visible as desired versus applied count.
See the [external sample](../config/samples/capacity-external.yaml).
