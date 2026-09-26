---
title: Upgrade the runtime
description: Qualify the exact fork image, then roll it out one member at a time, or stop the fleet when the versions cannot run together.
---

Use a compatible digest-pinned fork and qualify its recovery/storage-format
compatibility with the currently deployed version. Every possible recovery
participant must understand the fork's native `bucket_complete` proof. A
matching image-name pattern does not establish compatibility.

The [recorded `.2 → .3` upgrade](../../qualification/runtime-upgrade/) passes
for both profiles, with 12/12 acknowledged values preserved in each fleet.
That is graceful maintenance evidence, not permission to use `.2` for crash
recovery or proof that other source/target pairs are compatible. Use the
[current artifact](../../reference/compatibility/) for new fleets.

Most runtime changes roll. Some pairs cannot run together in one fleet and need
a [full stop](#full-stop-upgrade) instead. The
[compatibility reference](../../reference/compatibility/) says which.

## Rolling upgrade

Change only the image:

```bash
kubectl --context YOUR_CONTEXT -n fleets patch celldfleet my-fleet   --type merge -p '{"spec":{"runtimeImage":"ghcr.io/ewhauser/celld@sha256:YOUR_VERIFIED_DIGEST"}}'
```

Replace `YOUR_VERIFIED_DIGEST` with the actual 64-character lowercase digest
before running the command. Members are replaced one at a time, so old and new
versions run together until the rollout completes; qualify that mix. No
downtime permission is needed.

A PersistentFleet member keeps its disk. The StatefulSet restarts one member at
a time, highest ordinal first, and waits for each to be Ready before the next;
see the [PersistentFleet lifecycle](../../concepts/current-operation/).

## Full-stop upgrade

A PersistentFleet moving from a `0.5.1-ewhauser` build to a `0.6.0-ewhauser`
build needs a full stop. A 0.6.0 member recovers its previous session only
from followers that return the ranged tail format, which 0.5.1 members cannot
send. A rolling update would stall on its first member. Bucket fleets have no
followers and use the rolling upgrade above.

The fleet serves nothing from step 2 until its members are Ready again. Each
member keeps its disk, drains on `SIGTERM` and recovers from that disk.

```bash
CONTEXT=YOUR_CONTEXT
NAMESPACE=fleets
FLEET=my-fleet
IMAGE=ghcr.io/ewhauser/celld@sha256:YOUR_VERIFIED_DIGEST
FLEET_UID=$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get celldfleet "$FLEET" -o jsonpath='{.metadata.uid}')

# 1. Pause the fleet. The operator stops changing its workload and replacing members.
kubectl --context "$CONTEXT" -n "$NAMESPACE" patch celldfleet "$FLEET" --type merge \
  -p '{"spec":{"maintenance":{"paused":true}}}'
kubectl --context "$CONTEXT" -n "$NAMESPACE" wait "celldfleet/$FLEET" \
  --for=condition=MaintenancePaused --timeout=2m

# 2. Stop every member, and wait until no member Pod remains.
kubectl --context "$CONTEXT" -n "$NAMESPACE" scale statefulset "$FLEET" --replicas=0
while kubectl --context "$CONTEXT" -n "$NAMESPACE" get pods \
  -l "celld.eric.dev/fleet-uid=$FLEET_UID" -o name | grep -q .; do sleep 5; done

# 3. Set the new image and resume in one change. Every member starts on it at once.
kubectl --context "$CONTEXT" -n "$NAMESPACE" patch celldfleet "$FLEET" --type merge \
  -p "{\"spec\":{\"runtimeImage\":\"$IMAGE\",\"maintenance\":{\"paused\":false}}}"
kubectl --context "$CONTEXT" -n "$NAMESPACE" wait "celldfleet/$FLEET" \
  --for=condition=Ready --timeout=20m
```

Do not change the image before every member Pod is gone, and do not resume
without the new image: either starts some members on each version. Step 3
restores the fleet's declared member count, including when `spec.capacity` is
set. The maintenance Kind suite runs these commands against a fleet under
write load and requires every acknowledged write to be readable afterwards.

Neither profile has an old release adapter or automatic rollback. Watch the
conditions and [troubleshoot waits](../../troubleshoot/lifecycle/).
