# ADR 0013 Observed capacity and journaled automatic intent

Status: Implemented experimental policy; production contraction remains blocked

## Authority

`spec.capacity` is optional and mutable. Omission retains manual operation;
`capacity: {}` defaults to Shadow. Shadow records recommendations but never
executes them. ScaleOut explicitly opts into bounded additions; Automatic also
requests scale-in through the existing qualification gates. The shipped manager
still cannot contract either profile. This does not qualify pressure relief,
durability, retained-EBS recovery, fencing or follower AZ diversity.

The lifecycle journal remains the only replica writer. The policy never changes
`spec.replicas`, workload templates or `/scale`. The reservation hash excludes the
new mutable policy, preserving the exact hash for pre-policy reservations. The
original replica baseline, PVC identities, loss fence and historical recovery
sessions retain step 3 semantics. No main/previous checkout migration is required.

In policy modes, the journal owns applied capacity; `spec.replicas` remains a
manual command field. A change to its value wins the next intent and resets
stabilization. Its pending manual target is persisted before intent so a restart
cannot discard it. It is not a permanent policy floor: use minReplicas for that.
Manual requests retain the existing 1–100 and AZ constraints and are not limited
by automatic policy bounds. Rewriting the same value is not a new command.
Removing `capacity` explicitly returns ownership to `spec.replicas`, possibly
requesting gated contraction. To pause automation at current capacity, use Shadow.

Before recording automatic intent, the controller rereads the fleet to discard
changes that occurred during collection. Edits are asynchronous requests, not a
transactional cancellation barrier: an edit racing the final authorization write
can still overlap an accepted intent.

Policy/manual edits never retarget, cancel or reuse a recorded operation UUID.
An already recorded addition finishes its original target even if mode changes
or maxReplicas is lowered. New actions use the new bounds after completion and a
new stabilization window. A recorded automatic removal freezes on a changed policy
or manual baseline; it cannot safely be canceled while a delayed issuer exists.
It also rechecks full fresh low-demand observations and the continuous stable
window before issuance; intervening pressure or missing data requires a new
window even with an existing intent. This is in addition
to every existing lifecycle/recovery gate. Recovery of an already issued removal
continues regardless of policy mode. There is no automatic operation timeout into
success, nor an administrator flag to qualify contraction.

## Observations

The installed collector performs only GET `/state` on each selected Pod's private
IP and GET `metrics.k8s.io/v1beta1/namespaces/<ns>/pods/<name>`. No Prometheus,
Kubernetes Pod proxy, runtime mutation or S3 data is used for load collection.
Prometheus remains optional for controller-runtime operational metrics.

Collection has an overall 10-second deadline, eight workers, individual two-second
request deadlines, a 100-Pod inventory ceiling and a 1 MiB runtime response limit.
Redirects and environment HTTP proxies are disabled for private runtime reads.
PodMetrics decoding checks kind, API version, namespace, name, container name,
required CPU/memory quantities and timestamp/window. Requests are read-only, with
get/list on core Pods and get-only on metrics Pods; no node metrics privileges.

Samples carry separate runtime source/receipt and Metrics Server source/receipt
timestamps plus the CPU measurement window. Runtime semantics use the pinned
v0.5.0 adapter. Metrics windows must begin after the observed container start.
The collector binds observations to Pod UID, container ID and restart count,
then rereads each Pod to reject replacement, restart, changed address, readiness
or fleet label. This is a load identity check, not a process-fencing certificate.
Only the pinned single celld container is accepted. Pod Ready and a fresh runtime
sample together define *observed useful* capacity; neither proves durability.

Both scaling directions require 100% of expected replicas with fresh runtime and
CPU/memory readings. Coverage is deliberately not relaxable in this version.
Missing samples, API/RBAC failures, partial inventory, malformed schemas, future
or stale clocks, repeated source timestamps and discontinuous collection reset
stabilization. Missing measurements never become zero demand. Every node's two
source timestamps must advance before another sample can count. Observations
between counted sample intervals still invalidate a stable window on changed
demand, unavailable readiness or invalid/incomplete metrics; they never advance
the positive sample count or source watermarks. Gaps larger than
maxAgeSeconds restart the window. No extrapolation across controller downtime.
Unknown data affects policy only; provisioning, manual operations and ordinary
status reconciliation retain their existing paths.

## Pure policy and persistence

The deterministic function takes configuration, retained state, observation and
current journaled count. High CPU, high memory, runtime pressure/lack of memory
headroom or activation/capacity/restoring backlog on any replica triggers a
scale-out candidate. CPU and memory use absolute per-container values, not a mean
reduced by idle/unready joiners. Being below minReplicas also requires the same
fresh, stable observation window before a bounded addition. Scale-in requires
all replicas below both low thresholds, without pressure or backlog, for its
entire stabilization interval. It recommends at most one removal. Values are
prototype defaults, not benchmarked production thresholds.

The retained reservation journal stores configuration and membership fingerprints,
source timestamp watermarks, consecutive-window starts/counts, last observation,
last durable action, pending start, manual baseline/target and the last decision.
Membership or policy edits reset stabilization but retain last-action and pending
timing. Status is only a projection. Intent and last-action timing are saved in
the same optimistic journal update before replica changes; shadow recommendations
do not consume action budget. Both directions enforce cooldowns since any durable
capacity action, including manual actions while policy history exists.

All unready/unobserved replicas count as pending, including replicas requested by
an operation but not issued yet. Any pending capacity holds additional scaling;
it never counts as useful or reduces measured incumbent pressure. Provisioning
beyond the configured timeout reports IneffectiveCapacity and names scheduling,
PVC/AZ availability and pressured-incumbent join readiness as possible causes.
This is a diagnostic deadline, not evidence of success. It clears only when the
entire applied inventory has fresh runtime observations and readiness. The
operator neither bypasses celld readiness nor adds indefinitely to relieve a
join gate that scale-out cannot fix.

Optimistic reservation updates arbitrate conflicting decisions. Replica issuance
continues through the original workload resourceVersion/operation-ID CAS. The
sticky workload loss fence still invalidates delayed removal issuers; no policy
metric, threshold, minimum or history reset can bypass it. Policy history is
bounded to the current inventory's watermarks. Existing lifecycle/session history
is not pruned; annotation capacity exhaustion fails closed before further action.
