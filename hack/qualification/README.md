# Control-plane wire fixture

`controlplane-fixtures.py` compiles the fork's actual strict-shutdown serializer
and control state into a small offline harness:

```sh
python3 hack/qualification/controlplane-fixtures.py /absolute/path/to/celld
```

It requires Cargo and cached serde_json dependencies and updates
`internal/runtime/controlplane/testdata/shutdown-v1.json`. It never runs celld or
qualifies recovery. Source hashes and the wire contract are recorded in
[the typed client documentation](../../docs/runtime-control-plane.md).

Live integration belongs in `hack/integration` and the opt-in runtime tests under
`internal/launcher` and `internal/controller`. The old private-S3 snapshot/replay,
stock-version and disk-reuse harnesses have been removed.
