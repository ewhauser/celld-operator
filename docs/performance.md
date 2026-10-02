# Operator performance sweep

This sweep compares the operator at `8228631` with the local performance fixes.
The original sweep started at `8a2a69b`; measurements and compatibility checks
were refreshed after rebasing onto current main's mesh and export support.
CPU and heap allocation profiles cover API validation, capacity evaluation,
resource rendering/comparison, steady reconciliation, contraction, collection,
preview seeding, preview event fanout, routing and runtime `/state` decoding.
The executable source in `cmd/`, `api/`, `internal/`, and the integration and
release tooling in `hack/` was inspected. The static documentation site was
reviewed for repeated work; it has no operator runtime hot path.

## Fixes

- Compile validation and digest regexes once, including export bucket names.
  Check the fixed image digest
  directly while retaining the CRD's repository grammar.
- Validate each untrusted `/state` response once, with strict duplicate-key,
  escaped-name, nesting and trailing-content checks. Reuse that validation for
  nested objects, and decode application generations without per-cell JSON
  buffers. Unknown schemas and invalid numeric samples remain conservative.
- Read a shared ReplicaSet once per membership walk instead of once per Pod.
  Every Pod's owner UID is checked, and the next walk reads authority again.
- Read existing permanent reservations before attempting creation. A missing
  reservation still uses atomic Create, and a concurrent winner is read into
  an empty object and checked before any workload is adopted or created.
- Validate contraction freshness and membership once per observation, then
  check every possible victim against its survivors.
- Choose empty workload read objects without rendering discarded templates,
  including mesh membership reads.
  Compact owned Pod lists in place and avoid copying unrelated fleet fields
  while verifying reservation hashes.
- Normalize only the compared resource specs; avoid copying metadata, status
  and managed fields. Comparison leaves both input objects unchanged.
- Retain intermediate capacity watermarks without building a map that will be
  discarded. Bound capacity map hints and remove temporary metrics buffers.
- Bound metrics bodies before client-go decodes them, including error bodies.
  Reads stop at 1 MiB plus one byte, the original body is closed, and oversize
  metrics never supply capacity evidence.
- Keep pointers to Pods during application membership checks and reject
  duplicate entries. Index preview pool references in the manager cache for
  event fanout; reconciliation continues to use direct authority reads.
- Traverse routing status without copying it, and compare parent identities
  with the same API defaults and rejection of unexpected fields.

## Measurement method

Measurements use Go 1.27.1 on Darwin arm64, Apple M3 Max. Benchmarks cover 3 and
100 replicas, 1 and 100 seed objects, and 0, 1,000 and 10,000 resident cells.
Preview fanout compares 100 and 10,000 previews with ten matches. Metrics tests
include a 4 MiB response under HTTP 200 and 403.

CPU profiles and allocation profiles (`alloc_space` and `alloc_objects`) are
separate from uninstrumented timing comparisons. The baseline uses the same
benchmark fixtures on the original source. The baseline-only donor adapter
calls the original per-victim validation loop; it does not change baseline
production behavior. Both collector fixtures use fresh runtime timestamps and
assert that runtime and Kubernetes metrics were accepted.

Reconciliation fixtures use controller-runtime's fake client, whose object
serialization and REST-mapper work contribute to the measurements. Collector
and runtime HTTP benchmarks use in-memory transports. Preview fanout uses a
real client-go index and models result materialization; the actual manager
cache is checked against envtest. These results quantify local work, excluding
production API, network, AWS and runtime storage latency.

CPU profiles identify regexp matching/compilation and repeated JSON token
traversal alongside allocation and garbage collection work. The baseline
runtime allocation profile highlights `uniqueValue`, `json.Decoder.Token`
and their scratch maps; the optimized production path removes the recursive
validator and per-cell decoders. Controller allocation profiles also identify
discarded workload rendering, copied resource metadata and full metrics reads.
Remaining runtime allocations include strict duplicate-name bookkeeping and
decoded maps. Profile totals are not compared as speedups because adaptive
benchmarks execute different numbers of iterations.

## Results

Medians of three alternating before/after rounds, `GOMAXPROCS=2`, 200 ms
per benchmark, without profiling. Time/op measures local elapsed work; CPU
profiles supply attribution. These are local fixture results, not production
throughput estimates. Full samples and allocation counts are in
`bin/performance/pr-comparison/comparison.json`.

| Workload | Before time/op | After time/op | Before bytes/op | After bytes/op |
|---|---:|---:|---:|---:|
| Fleet validation | 24.76 us | 1.04 us | 47,623 | 0 |
| Fleet validation with export bucket | 42.47 us | 1.15 us | 89,564 | 0 |
| Collector, 100 replicas | 4,154.02 us | 3,653.86 us | 4,190,463 | 2,854,191 |
| Runtime application, 1,000 cells | 938.99 us | 450.66 us | 810,453 | 420,923 |
| Runtime capacity, 1,000 cells | 413.33 us | 247.68 us | 410,811 | 273,838 |
| Bucket reconciliation, 100 replicas (fake API) | 4,505.85 us | 2,773.07 us | 3,305,540 | 1,960,986 |
| Persistent reconciliation, 100 replicas (fake API) | 6,922.59 us | 5,540.44 us | 5,837,167 | 3,934,809 |
| Deployment contraction, 100 replicas (fake API) | 6,891.07 us | 2,482.60 us | 3,801,367 | 1,484,716 |
| All-victim validation, 100 replicas | 3,024.93 us | 91.12 us | 1,339,200 | 6,992 |
| Seed receipt, 100 objects | 1,449.00 us | 163.03 us | 1,879,661 | 40,934 |
| Application membership, 100 Pods | 83.48 us | 21.83 us | 139,496 | 8,296 |
| Routing readiness, 32 parents | 56.42 us | 3.55 us | 65,688 | 48 |
| Oversize metrics response, 4 MiB | 910.54 us | 381.94 us | 10,041,223 | 2,267,974 |
| Intermediate capacity evaluation, 100 replicas | 32.28 us | 16.79 us | 25,004 | 12,114 |

The preview index model with 10,000 previews and ten matches reduces result
materialization from 10,405,088 to 6,568 bytes/op and time from 4,380.85 to
3.09 us/op. A membership walk with 100 Pods now
reads its shared ReplicaSet once rather than 100 times. Stable fleet and
preview-pool reservations perform zero Create calls.

Runtime application observation reduces allocation count from 12,642 to
3,232 per operation; the 100-replica collector reduces it from 67,375 to 25,761.

The isolated, test-only `parseLoad` benchmark with a 1,000-cell census allocates
17% more bytes because native duplicate-name bookkeeping uses more space.
The complete production `State`/`Capacity` path improves time, allocated bytes
and allocation count by avoiding repeated validation. Resource construction
has unchanged allocations; its timing difference is treated as noise. The
3-replica collector's median time is 3.6% higher while bytes fall 31.5%; small
timing changes and unchanged control cases vary on this host and are not
interpreted as performance improvements.

Instrumentation substantially perturbed timings on this macOS host. Initial
contended runs and profiled timings were excluded from the results table.
Profiles are used to identify work and allocations, not to estimate speedups.

## Verification

- `make check` passed after rebasing onto `8228631`: all-package build, race
  tests, and native/Linux linters with zero issues.
- Full `TestEnvtest*` suite passed against kube-apiserver/etcd 1.37.0, including
  the new manager-cache index, admission, reservation conflicts and routing.
- `make manifests-check` confirmed that generated CRDs and deepcopy code match.
- JSON validation differential fuzz: 259,503 inputs; runtime image grammar
  differential fuzz against the CRD regex: 153,453 inputs. Both passed.
- `bash -n hack/profile-performance.sh` and `git diff --check` passed.
- Linux controller test binary cross-compiled. Container execution could not
  run because the Docker daemon was stopped. No Linux runtime-test pass is claimed.

Final PR validation receipts are kept as `bin/performance/check-pr-final.txt`,
`envtest-pr.txt` and `manifests-pr.txt`; `test-linux.txt` records the unavailable
container execution from the original sweep.

## Reproduction

Run all benchmarks without profiling:

```sh
GOMAXPROCS=2 go test ./api/v1alpha1 ./internal/capacity ./internal/controller ./internal/runtime/controlplane \
  -run '^$' -bench . -benchmem -benchtime=1s -count=5
```

Collect CPU and allocation profiles and readable hotspot tables:

```sh
hack/profile-performance.sh bin/performance
go tool pprof -http=127.0.0.1:0 bin/performance/internal-controller.cpu.pprof
go tool pprof -http=127.0.0.1:0 -sample_index=alloc_space \
  bin/performance/internal-controller.allocs.pprof
```

The script accepts an output directory and `BENCHTIME`/`GOMAXPROCS` overrides.
Generated profiles and test binaries stay under ignored `bin/`. The sweep's
before/after profiles and matching binaries are kept under
`bin/performance/pr-before/` and `pr-after/`; uninstrumented timing receipts
are under `bin/performance/pr-comparison/` in this checkout.

## Safety and scope

Regression coverage checks reservation races, per-Pod ownership, freshness and
every-victim contraction rejection, immutable comparison inputs, bounded
success/error metrics, duplicate application membership, routing defaults and
actual cache indexing. JSON and image grammar changes were also differentially
fuzzed against the original implementations.

No additional changes were justified in resource construction, collector
sorting, redistribution, maintenance, ordered scheduling, process startup,
integration orchestration or release tooling. Read-before-mutate checks and
per-member foreign-claim reads remain intentional safety work. No live-cluster
throughput, Kubernetes scheduler behavior, runtime durability or AWS scaling
qualification is inferred from these local profiles.
