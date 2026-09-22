# Documentation

The [user guide](../site/src/content/docs/start/overview.md) covers installation,
configuration and operations. The [capability matrix](critical-features.md)
describes implemented behavior and operational limits.

The implementation has three authority boundaries:

- [celld control plane](runtime-control-plane.md): generation-bound data safety.
- [Launcher](launcher-supervision.md): exact process exit and restart exclusion.
- [Current operation](current-operation.md): bounded Kubernetes authority and conditional infrastructure changes.

Additional contributor references:

- [Fleet API](fleet-api.md), [operations](operations.md) and [capacity policy](capacity-policy.md).
- [Ordered Bucket layout](ordered-bucket.md) and [runtime requirements](runtime-versions.md).
- [Architecture decision](decisions/README.md) and [qualification](qualification/README.md).

Superseded journal, recovery-metadata and fencing implementations and their test
artifacts are available in Git history. They are not supported deployment paths.
