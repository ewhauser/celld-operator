# Ordered Bucket fleets

`spec.profile: Bucket` with `spec.bucketWorkload: Ordered` creates a StatefulSet
with disk-backed `emptyDir`. It creates no PVCs. celld's node identity is still
the Pod UID. Both Bucket layouts run celld directly with
`CELLD_DURABILITY=bucket`: no launcher, strict proof or current operation.

Strict placement releases a scheduling gate only after assigning ordinal `n` to
`placement.zones[n % azCount]`. Required hostname anti-affinity keeps members
on distinct nodes. A retained ordinal prefix covers the configured zones;
contraction cannot go below the zone count. Relaxed placement keeps the zone
allowlist and soft spread preferences.

Contraction lowers replicas by one, removing the highest ordinal, and waits for
the StatefulSet to report updated, ready replicas before the next step.
Automatic and External contraction also require survivor-capacity evidence for
that ordinal. Restart and upgrade use a StatefulSet `RollingUpdate`, one ordinal
at a time. New growth starts fresh Pods and temporary disks.

The default Bucket layout is `Deployment` (`RollingUpdate`, `maxUnavailable: 1`,
`maxSurge: 0`). It contracts the same way, but Kubernetes chooses the victim, so
automatic contraction requires survivor capacity for every member. Layout is
immutable; choose Ordered at creation for deterministic zone assignment.

See [qualification](qualification/README.md) for actual local and cluster coverage.
