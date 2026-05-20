#!/bin/sh

set -e

COUNT="${COUNT:-5}"
BENCHTIME="${BENCHTIME:-2s}"
PROFILES="${PROFILES:-0}"
OUTPUT_DIR="${OUTPUT_DIR:-bench/outputs}"
FULL_RUN_FILE="$OUTPUT_DIR/full_run.txt"
SUMMARY_FILE="$OUTPUT_DIR/summary.txt"

if ! command -v go > /dev/null 2>&1; then
    echo "Go compiler not found in PATH. Install Go and retry." >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

mkdir -p "$OUTPUT_DIR"
: > "$FULL_RUN_FILE"
: > "$SUMMARY_FILE"

run_bench() {
    name="$1"
    pkg="$2"
    pattern="$3"

    out_file="$OUTPUT_DIR/$name.txt"
    args="test $pkg -run ^$ -bench $pattern -benchmem -count $COUNT -benchtime $BENCHTIME"

    if [ "$PROFILES" = "1" ]; then
        cpu="$OUTPUT_DIR/$name.cpu.pprof"
        mem="$OUTPUT_DIR/$name.mem.pprof"
        args="$args -cpuprofile $cpu -memprofile $mem"
    fi

    echo "Running benchmark: $name"
    # shellcheck disable=SC2086
    go $args | tee "$out_file" | tee -a "$FULL_RUN_FILE"
}

extract_ns_op() {
    file="$1"
    pattern="$2"

    line="$(grep "$pattern" "$file" | tail -n 1 || true)"
    if [ -z "$line" ]; then
        echo ""
        return 0
    fi

    echo "$line" | awk '{for (i = 1; i <= NF; i++) if ($i == "ns/op") {print $(i-1); exit}}'
}

write_revrel_mode_summary() {
    hotspot_file="$1"
    unbuffered_pattern="BenchmarkIngestMetadataPathHotspots/process_revrel_dual_write_unbuffered/files_32/chunks_16"
    buffered_pattern="BenchmarkIngestMetadataPathHotspots/process_revrel_dual_write_buffered/files_32/chunks_16"

    unbuffered_ns="$(extract_ns_op "$hotspot_file" "$unbuffered_pattern")"
    buffered_ns="$(extract_ns_op "$hotspot_file" "$buffered_pattern")"

    {
        echo ""
        echo "=== Reverse-Relation Dual-Write Mode Summary ==="
        if [ -z "$unbuffered_ns" ] || [ -z "$buffered_ns" ]; then
            echo "Buffered/unbuffered hotspot lines not found in $hotspot_file"
        else
            improvement="$(awk -v u="$unbuffered_ns" -v b="$buffered_ns" 'BEGIN { if (u > 0) printf "%.2f", ((u-b)/u)*100; else printf "0.00" }')"
            echo "unbuffered_ns_op: $unbuffered_ns"
            echo "buffered_ns_op: $buffered_ns"
            echo "buffered_improvement_percent: $improvement"
        fi
    } | tee "$SUMMARY_FILE" | tee -a "$FULL_RUN_FILE" > /dev/null
}

run_bench "service_getchonkmap" "./lib/service" "^BenchmarkGetChonkMap$"
run_bench "store_ingest_path" "./lib/store" "^BenchmarkIngestDataPath$"
run_bench "store_ingest_metadata_path" "./lib/store" "^BenchmarkIngestMetadataPath$"
run_bench "store_ingest_metadata_hotspots" "./lib/store" "^BenchmarkIngestMetadataPathHotspots$"
run_bench "store_revrel_path" "./lib/store" "^BenchmarkProcessRevRel$"
run_bench "store_repair_scan" "./lib/store" "^BenchmarkInspectEvidenceRepairs$"
run_bench "store_restore_path" "./lib/store" "^BenchmarkRestoreDataPath$"

write_revrel_mode_summary "$OUTPUT_DIR/store_ingest_metadata_hotspots.txt"

echo "Benchmark run complete. Outputs in $OUTPUT_DIR"
echo "Summary: $SUMMARY_FILE"