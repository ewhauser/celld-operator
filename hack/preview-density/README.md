# Preview density experiment

This is a local Docker/MinIO investigation, not Kubernetes qualification.
Run it only on a development Docker host with several GiB of spare memory.
The runner creates a uniquely named network, disposable local credentials,
MinIO, an S3 request-counting proxy, and isolated fleet containers. It cleans up
only its own containers/network in `finally`. Data in its MinIO is disposable.
Do not interrupt with SIGKILL; if forcibly interrupted, use the exact run label
recorded in the JSON to identify leftovers. Never run a global Docker prune.

The pinned images here are the **arm64 images used in this investigation**.
Use a native Linux arm64 Docker host (including Colima on Apple Silicon).

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/preview-density-launcher ./cmd/celld-launcher
python3 hack/preview-density/run.py --mode smoke --output /tmp/preview-smoke.json
python3 hack/preview-density/run.py --mode limits --output /tmp/preview-limits.json
python3 hack/preview-density/run.py --mode density --counts 10 30 50 100 --output /tmp/preview-density.json
python3 hack/preview-density/run.py --mode quiet --output /tmp/preview-quiet.json
```

The Docker context must use a Unix socket and share the repository directory.
No production credentials, Kubernetes context or existing application is used.
`--seconds` controls idle windows (default 65); `quiet` uses at least 185 seconds.
`--limits` selects memory ceilings for the stateful test (default 128 256 MiB).
The tested 64 MiB ceiling stalled on this workload and is not recommended.
Density startup refuses the next target when estimated container working memory
would exceed 75% of VM RAM. This is a coarse guard, not a guarantee against
concurrent unrelated workloads or unsampled peaks. Launches and stops use at
most 16 concurrent Docker operations; load uses four concurrent fleet clients.

Each runtime has a separate object-store prefix. The fixture deliberately uses
the same script name, DO class and logical cell names in different fleets, with
preview-specific values. It writes and checks 128 KiB per object. The `small`
profile limits stateless isolates to 1, requests to 8, cell requests to 4,
resident cells to 8, idle residency to 30 seconds and request bodies to 1 MiB.
`quiet` additionally uses a 60-second lease TTL, 300-second deployment poll and
disabled balancing; it is an experiment, not an operator-supported profile.

To measure the shared store/proxy as well, run the following while a density
run is active, using its exact `run` value from the JSON:

```sh
python3 hack/preview-density/observe.py --run preview-density-EXACT_RUN_ID --output /tmp/preview-services.json
```

The observer samples cgroup service cost every ten seconds and exits when that
run's containers disappear (or after thirty minutes). It does not probe apps.

JSON includes Docker capacity, launcher hash, startup times, cgroup snapshots,
health response counts, counted S3 operations, write/read outcomes, graceful
stop logs and fresh-filesystem recovery. Memory is Docker/cgroup working set
(usage minus inactive file cache), not process RSS. CPU is cgroup CPU time over
wall time. Stats are sampled approximately every 10 seconds; these are sampled
peaks, not peak-allocation measurements. Health probes run about every 2 seconds.
The local Python client and accounting proxy affect latency; no production
throughput claim should be based on these tests.

See [the archived report](../../docs/qualification/preview-density/README.md)
and [the design investigation](../../docs/research/cheap-dedicated-previews.md).
