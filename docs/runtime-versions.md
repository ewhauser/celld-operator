# Runtime requirements

Provisioning requires an explicit registry-pinned OCI image ending in
`@sha256:<64 lowercase hex digits>`. There is no default runtime image and no
list of stock upstream releases accepted as substitutes.

The required fork is based on upstream v0.6.1 and exposes its state through the
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

## Change export

`spec.export` needs a celld with change export, which starts at
`v0.6.1-ewhauser.1`. Earlier builds, including `v0.6.0-ewhauser.2`, ignore the
`CELLD_EXPORT_*` settings and export nothing. The default release image carries
only the bucket sink. Starting with `v0.6.1-ewhauser.3`, the `-kafka` image
variant includes both the bucket sink and the Kafka sink (`export-kafka`).
Pin that variant's verified manifest digest for `spec.export.sink: Kafka`.
Compiling in Kafka support does not enable export; `spec.export` still controls
whether and where changes are exported.

## Published fork artifact

Release `v0.6.1-ewhauser.3` publishes a Kafka-enabled Linux amd64/arm64
image index, used by the operator's integration and release qualification:

```text
ghcr.io/ewhauser/celld@sha256:d880dae9f8e14d55740fbcf361d01e32cefca113b631a6e9cf99f5f1f1edbc4e
```

This is the `v0.6.1-ewhauser.3-kafka` variant, built with `export-kafka`.
It supports the bucket and Kafka sinks; export remains disabled unless
configured. The default release image omits Kafka. The source revision is
`5fa3bd04cefd08169dd49a9cf9cf46d65faaa775`. The image is also tagged
`sha-5fa3bd04cefd08169dd49a9cf9cf46d65faaa775-kafka`.
Native default and Kafka binaries, build records, and checksums are attached to
the [fork release](https://github.com/ewhauser/celld/releases/tag/v0.6.1-ewhauser.3).

The release retains the `0.6.1-ewhauser.2` storage and export improvements.
Kafka artifact availability introduces no storage-format change; existing
`0.6.1-ewhauser` fleets can roll to this runtime. Follow the source/target
qualification requirements above before upgrading a deployed fleet.

### v0.6.1-ewhauser.1

Release `v0.6.1-ewhauser.1` has a Linux amd64/arm64 image index:

```text
ghcr.io/ewhauser/celld@sha256:e8dc139226269acf137b54b67cea42a3b6a7c6527ce968caa5d303c21c3b7b74
```

It is `0.6.0-ewhauser.2` moved onto upstream v0.6.1 (Python Workers, epoch GC
through `CELLD_LTX_RETENTION_SECS`, configurable asset and Dynamic Worker size
limits), plus change export and an optional DynamoDB control plane, both off by
default. The operator sets neither `CELLD_CONTROL` nor the new upstream
settings. Nodes roll from `0.6.0-ewhauser.2`. Set `CELLD_LTX_RETENTION_SECS`,
deploy a Python Worker, or raise `CELLD_MAX_ASSET_FILE_BYTES` above 25 MiB only
after every member runs this build.

The source revision is `350202332f861e52b4f88b67670d1c0862d1cf59`; the index is
also tagged `sha-350202332f861e52b4f88b67670d1c0862d1cf59`. Native binaries and
checksums are attached to the
[fork release](https://github.com/ewhauser/celld/releases/tag/v0.6.1-ewhauser.1).

### v0.6.0-ewhauser.2

```text
ghcr.io/ewhauser/celld@sha256:94f35ba6973942eb4aaad2830ee8ebe431e2a125940acf380e74782602980a2a
```

It is `0.6.0-ewhauser.1` plus two changes. celld exports OTLP metrics beside
traces and logs and reads `OTEL_RESOURCE_ATTRIBUTES`, which the operator sets
when `spec.telemetry` is present (see [fleet API](fleet-api.md)). Strict
disk-removal shutdown and `/state.node_log` are removed; the operator uses
neither, and `/state.shutdown` keeps `schema_version: 1` and
`runtime_generation`. Nodes roll from `0.6.0-ewhauser.1` with no record or
storage format change.

The source revision is `70a1e0b7fa16a3bccdc739331c9231f6ddc0a299`; the index is
also tagged `sha-70a1e0b7fa16a3bccdc739331c9231f6ddc0a299`. Native binaries and
checksums are attached to the
[fork release](https://github.com/ewhauser/celld/releases/tag/v0.6.0-ewhauser.2).

### v0.6.0-ewhauser.1

```text
ghcr.io/ewhauser/celld@sha256:3e6c45392310add318952e45427db3316251a912ea7fd8d2fbff438fd2cc9f7f
```

It is upstream v0.6.0 with every fork change through `0.5.1-ewhauser.7`. A
PersistentFleet on a `0.5.1-ewhauser` build moves to it with a
[full stop](../site/src/content/docs/operate/upgrade-runtime.md#full-stop-upgrade),
not a rolling update: a 0.6.0 member recovers its previous session only from
followers that return the ranged tail format, which 0.5.1 members cannot send.
Bucket fleets roll.

From `.7`, a member on a replacement disk answers recovery for its own old disk
with a conclusive "no fragment" instead of refusing it. Members that lose their
disks at the same time then recover each other's sessions, or record a bounded
loss, instead of refusing each other forever. Another node at the member's
address is still refused. Upgrade every member before relying on this: an
older follower still refuses a replacement's answer.

From `.6`, three failed idle probes to a follower degrade the leader's ensemble
exactly as a failed write does, so an idle fleet also moves off a departed
member. Earlier builds move off it only after a failed write, so an idle leader
can keep depending on a removed member's disk; see the
[limits](current-operation.md#limits) before deleting one by hand.

The source revision is `fdd0cf585189c92dd1712c6aba685b4294e65a88`.
[Release build 36272411173](https://github.com/ewhauser/celld/actions/runs/36272411173)
and [image build 36273043419](https://github.com/ewhauser/celld/actions/runs/36273043419)
completed. The native binaries' checksums and recorded source commit,
`celld --version` on both image platforms, and the image index's build
attestation, issued to the tag's release workflow, were verified. Native
binaries and checksums are attached to the
[fork release](https://github.com/ewhauser/celld/releases/tag/v0.6.0-ewhauser.1).
Confirm deployment behavior with the exact artifact and storage configuration you use. The operator image is built and pinned separately.
