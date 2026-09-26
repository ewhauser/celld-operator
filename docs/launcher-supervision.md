# Launcher supervision (obsolete)

No fleet profile uses the launcher. Bucket and PersistentFleet Pods run celld
directly, with no launcher container, key Secret, port 8083, scheduling gate or
liveness probe. The operator accepts `--launcher-image` and ignores it so
existing deployments keep starting; the chart's `launcherImage` value is
likewise unnecessary.

Earlier releases ran PersistentFleet under a launcher that captured celld's
strict `remove-disk` result, proved exact child exit and inherited-lock release,
and wrote a permanent restart-deny marker on the disk before the operator
removed a member ([ADR 0022](decisions/0022-celld-control-plane.md)). PersistentFleet
is now a StatefulSet that restarts one member at a time on its retained disk;
see [PersistentFleet lifecycle](current-operation.md).

On upgrade, the StatefulSet replaces each earlier Pod with one from the current
template, including Pods still held by the launcher scheduling gate.
Restart-deny markers left on retained disks are ignored because nothing reads
them.

The `internal/launcher` package and `cmd/celld-launcher` binary have been removed,
and the operator image no longer contains the launcher. Their design and
September real-binary handshake evidence are in Git history and the
[qualification records](qualification/README.md).
