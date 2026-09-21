# Ordered Bucket fleets

`spec.profile: Bucket` with `spec.bucketWorkload: Ordered` creates a StatefulSet
with disk-backed `emptyDir`. It creates no PVCs. celld's node identity is still
the Pod UID. Both Bucket layouts use the strict launcher.

Strict placement releases a scheduling gate only after assigning ordinal `n` to
`placement.zones[n % azCount]`. Required hostname anti-affinity keeps members
on distinct nodes. A retained ordinal prefix covers the configured zones;
contraction cannot go below the zone count. Relaxed placement keeps the zone
allowlist and soft spread preferences.

The executor captures the highest ordinal's exact process identity, records a
strict removal operation and persists completed runtime/launcher proof before
a conditional replica update. It then verifies the intended Pod disappeared.
New growth starts fresh Pods and temporary disks. Unknown generation changes,
wrong victims or incomplete proof block the operation.

The default Bucket layout is `Deployment`. Its nondeterministic victim selection
cannot satisfy exact single-member removal, so contraction is blocked. Whole-fleet
maintenance may stop every captured member before changing its replica count.
Choose Ordered at creation if contraction is required; layout is immutable.

See [current operations](current-operation.md) for the complete effect protocol
and [qualification](qualification/README.md) for actual local and cluster coverage.
