#!/usr/bin/env bash
# Profile local operator work; timings from these instrumented runs are diagnostic.
set -euo pipefail

cd "$(dirname "$0")/.."
output="${1:-bin/performance}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"
export GOMAXPROCS="${GOMAXPROCS:-2}"

for package in api/v1alpha1 internal/capacity internal/controller internal/runtime/controlplane; do
    name="${package//\//-}"
    go test "./$package" -run '^$' -bench . -benchmem \
        -benchtime="${BENCHTIME:-1s}" -count=1 \
        -cpuprofile="$output/$name.cpu.pprof" \
        -memprofile="$output/$name.allocs.pprof" \
        -o "$output/$name.test" > "$output/$name.bench.txt"
    go tool pprof -top -nodecount=30 "$output/$name.cpu.pprof" \
        > "$output/$name.cpu.txt"
    go tool pprof -top -sample_index=alloc_space -nodecount=30 \
        "$output/$name.allocs.pprof" > "$output/$name.alloc-space.txt"
    go tool pprof -top -sample_index=alloc_objects -nodecount=30 \
        "$output/$name.allocs.pprof" > "$output/$name.alloc-objects.txt"
done

printf 'CPU and allocation profiles saved in %s\n' "$output"
