# Reproduce local qualification

Requires Docker, Python 3 with venv, network access to the pinned images/npm
binary, and the repository's Go 1.27.1 toolchain for replay. This harness supports
linux/arm64 and linux/amd64 images; only arm64 has been exercised here.

```sh
python3 -m venv .qualification-venv
.qualification-venv/bin/pip install -r hack/qualification/requirements.txt
make check
make qualification-replay
make qualification-test
make qualification-local SCENARIO=fleet OUTPUT=.qualification-runs/fleet-new
make qualification-local SCENARIO=bucket OUTPUT=.qualification-runs/bucket-new
make qualification-local SCENARIO=contraction OUTPUT=.qualification-runs/contraction-new
make qualification-local SCENARIO=pressure OUTPUT=.qualification-runs/pressure-new
make qualification-local SCENARIO=partition OUTPUT=.qualification-runs/partition-new
```

Every output directory must be new. Each scenario has a 420-second overall
budget, bounded HTTP/S3/Docker calls, one dedicated randomly named Docker
network and MinIO container, and unique retained volumes per runtime node.
Ports bind only to host loopback. It does not read AWS credentials, kubeconfig,
or a default cloud endpoint. Static credentials are public local test values;
boto3 is explicitly pointed to the new loopback MinIO endpoint. Docker runtime
containers receive only the local store settings, not host cloud credentials.
Do not change the harness to use a production endpoint.

The synthetic application's stable operation IDs make retries idempotent. Every
successful PUT acknowledgement is entered in `acknowledged-ledger.json`; after
faults, the client reads every acknowledged ID through a survivor. Requests are
sequential and tiny; this is a recovery experiment, not a capacity benchmark.
The pressure workload's concurrent cold reads add no untracked application
writes. Client-process crash durability and ambiguous non-idempotent retries
are not tested.

The released celld image has no esbuild. The harness downloads esbuild 0.25.12
for the Docker image architecture, verifies its pinned SHA-512, and mounts the
binary for deployment of the checked-in test app. This does not alter celld.
MinIO is pinned to a digest as well. It remains an unqualified local S3 substitute.

Outputs include full local logs, status/metadata captures, image identity,
readiness samples (latest harness), and measured results. Metadata collection
GETs only `nodes/` and lists `nodes/`/`log/`, fully paginating; it never downloads
application objects or loss/bundle bodies. The harness uses local administrative
credentials to provision/deploy; it does **not** qualify restricted IAM.

Containers and named volumes are removed in `finally`, including on ordinary
failure or the overall timeout. Volumes are retained across the restart within
each scenario. A hard kill of Python/host may bypass cleanup: locate the exact
`celld-q-<run-id>` names in that run's logs or `docker ps -a`, inspect them, and
remove only those containers, volumes and network. Never use Docker prune.
Existing kind/other containers are unrelated and must remain untouched.

To export reviewable evidence after a completed run:

```sh
python3 hack/qualification/export.py .qualification-runs/fleet-new docs/qualification/fleet-new
```

This exports synthetic operation IDs and recovery metadata; it omits binaries,
raw logs and application identifiers in `/state`, and records hashes/redactions.
Raw output is ignored by Git. Never promote a synthetic mutation to an observed
fixture. See `internal/runtime/v050/testdata/README.md` for fixture provenance.

`celld-qualify` is an offline replay tool. It takes a pre-disruption inventory,
a later completed capture, and **explicit** confirmed-stopped identities. The
clock is the capture time; success is historical candidate evidence, not current
authorization. Complete JSON files are trusted harness artifacts, not a live S3
transport. Example:

```sh
go run ./cmd/celld-qualify \
  -before docs/qualification/contraction/before-abrupt-metadata.json \
  -after docs/qualification/contraction/after-abrupt-metadata.json -stopped a,b
```

Known gate outcomes such as absent Bucket logs or pressured startup are recorded
without making the experiment itself fail. Setup/transport/ledger-read failures
fail the command; inspect `results.json` rather than treating exit 0 as runtime
qualification. The final client `missing` list must be empty, and positive
recovery observations require exact generation matches and no listed loss keys.
