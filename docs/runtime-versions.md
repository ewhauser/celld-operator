# Runtime requirements

Provisioning requires an explicit registry-pinned OCI image ending in
`@sha256:<64 lowercase hex digits>`. There is no default runtime image and no
list of stock upstream releases accepted as substitutes.

The required fork is based on upstream v0.5.1 and exposes its state through the
existing `/state` control plane. Every possible recovery participant must
understand its native `bucket_complete` proof. See
[the wire contract](runtime-control-plane.md) and [fork source](https://github.com/ewhauser/celld).

Pin validation is syntactic. Before deployment, verify the artifact's source,
platform and digest and verify a homogeneous fleet against the required API and
recovery behavior. A native binary checksum is not a container image digest.
An ECR pull-through cache or other mirror may be used, for example
`123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29`.
Confirm that the mirror serves the compatible fork under the pinned digest; the
operator cannot infer image provenance from the reference.

A runtime-image change is a rolling update for both profiles. PersistentFleet
replaces one member at a time on its retained disk, highest ordinal first, and
waits for each to be Ready, so old and new versions run together during the
rollout. No old runtime adapter or directional stock-release migration exists.
The caller must establish source/target format compatibility; the operator
cannot infer it from two valid digest strings.

The operator does not read celld's `/state.node_log`
([ADR 0024](decisions/0024-persistentfleet-is-a-statefulset.md)) and treats
every fork build the same way. Builds that predate `node_log`, such as
`0.5.1-ewhauser.3` and `.4`, restart, upgrade and scale like later ones, so an
older fleet can upgrade in place.

## Published fork artifact

Release `v0.5.1-ewhauser.6` has a Linux amd64/arm64 image index:

```text
ghcr.io/ewhauser/celld@sha256:07a81e72155890b36529756a3ecbb22045d94679b3001a0341cacc044337549a
```

In `.6`, three failed idle probes to a follower degrade the leader's ensemble
exactly as a failed write does, so an idle fleet also moves off a departed
member. Earlier builds move off it only after a failed write, so an idle leader
can keep depending on a removed member's disk; see the
[limits](current-operation.md#limits) before deleting one by hand.

The source revision is `801e98307157e962a57d5b0804dcf4a21ced008c`.
[Release build 36205466928](https://github.com/ewhauser/celld/actions/runs/36205466928)
and [image build 36205982955](https://github.com/ewhauser/celld/actions/runs/36205982955)
completed. The binary checksums, build provenance, `celld --version` and the
image index's build attestation were verified. Native binaries and checksums are
attached to the [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.6).
Confirm deployment behavior with the exact artifact and storage configuration you use. The operator image is built and pinned separately.
