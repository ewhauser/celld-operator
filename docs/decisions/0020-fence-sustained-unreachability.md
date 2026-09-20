# ADR 0020 Fencing requires sustained donor unreachability

Status: Implemented

Date: 19 September 2026

## Context

Opt-in EC2 fencing (`docs/infrastructure-fencing.md`) completes a PersistentFleet
contraction whose admitted writer became unreachable during `Stopping`. The
operator entered that path on **any** single `callLauncher` error while the
fleet carried `celld.eric.dev/fence-operation` naming the operation. That call
has a 3-second timeout, so one transient network blip during a routine
`Stopping` phase was enough to save durable fence intent. Intent is deliberately
irreversible: once recorded, later reconciles complete it, cordoning the node and
calling `TerminateInstances` even if the launcher came back healthy seconds
later. Nothing verified the node was actually dead, so the annotation behaved as
a standing authorization to terminate on the next packet loss.

## Decision

Track donor unreachability durably on the lifecycle operation as
`DonorUnreachableSince`, an additive optional field on the existing journal
version. During `Stopping`, a failing donor `callLauncher` sets it (from the
reconciler's injectable clock) when zero and saves the journal; a successful call
clears it and saves. New fence intent may only be requested when the donor has
been continuously unreachable for at least `fenceUnreachableWindow` of 60
seconds, measured from that field, and never on the first failing reconcile,
which by construction cannot have observed a window. While waiting, the fleet
reports the condition reason `InfrastructureFencing` with the elapsed
unreachability and the required window.

A fence receipt already recorded for the operation still continues on every
reconcile exactly as before: intent, once durable, completes regardless of later
reachability. Every safety check inside `ensureInfrastructureFence` (exact
admission identity, tags, dedication, cordon, disk mapping, positive
`terminated` proof) is unchanged. The annotation cannot record intent in any
path where the last launcher call succeeded.

## Rejected alternative

Gate fencing on `op.Stalled`. Rejected: `Stalled` means the 30-minute
`operationBudget` expired, which is a schedule signal rather than evidence about
the host. It would delay a legitimate fence of a genuinely dead node by up to 30
minutes while adding no safety over a sustained-unreachability window, and a
stalled operation whose donor is reachable would still be eligible.

## Consequences

A donor that is dead stays fenceable, one minute later than before. A flapping
or briefly partitioned donor no longer accumulates irreversible termination
intent, and the waiting condition makes the delay visible. The new field decodes
as zero in journals written by earlier binaries, and older binaries ignore it.
