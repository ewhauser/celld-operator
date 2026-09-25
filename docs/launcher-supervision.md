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
now disrupts one member at a time and waits for celld's own node-log reports;
see [one disruption at a time](current-operation.md).

On upgrade, Pods still held by the launcher scheduling gate are released and
rolled onto the current template. Restart-deny markers left on retained disks
are ignored because nothing reads them.

The `internal/launcher` package, `cmd/celld-launcher` binary and `make test-linux`
remain in the repository until they are removed. Their design and September
real-binary handshake evidence are in Git history and the
[qualification records](qualification/README.md).
