#!/bin/sh

set -e

COUNT="${COUNT:-5}"
BENCHTIME="${BENCHTIME:-2s}"
PROFILES="${PROFILES:-0}"
OUTPUT_DIR="${OUTPUT_DIR:-bench/outputs}"
STORE_DATASET="${STORE_DATASET:-}"
STORE_EXE="${STORE_EXE:-}"
STORE_COUNT="${STORE_COUNT:-3}"
IO_URING_DEPTHS="${IO_URING_DEPTHS:-256 512 1024 2048}"
STORE_DB_ROOT="${STORE_DB_ROOT:-bench/store_matrix}"
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

extract_store_seconds() {
    text="$1"
    printf '%s\n' "$text" | sed -n 's/.*Stored in:[[:space:]]*\([0-9][0-9.]*\)s.*/\1/p' | tail -n 1
}

write_io_uring_depth_summary() {
    if [ -z "$STORE_DATASET" ]; then
        return 0
    fi

    if [ ! -f "$STORE_DATASET" ]; then
        echo "Store dataset path not found: $STORE_DATASET" >&2
        exit 1
    fi
    if [ "$STORE_COUNT" -lt 1 ]; then
        echo "STORE_COUNT must be >= 1" >&2
        exit 1
    fi

    mkdir -p "$STORE_DB_ROOT"

    {
        echo ""
        echo "=== io_uring Queue-Depth Sweep (store command) ==="
        echo "dataset: $STORE_DATASET"
        if [ -n "$STORE_EXE" ]; then
            echo "executable: $STORE_EXE"
        elif [ -x "./dues" ]; then
            echo "executable: ./dues"
        else
            echo "executable: go run ."
        fi
        echo "runs_per_depth: $STORE_COUNT"
    } | tee -a "$SUMMARY_FILE" | tee -a "$FULL_RUN_FILE" > /dev/null

    best_depth=""
    best_median=""

    for depth in $IO_URING_DEPTHS; do
        if [ "$depth" -le 0 ] 2>/dev/null; then
            echo "All IO_URING_DEPTHS must be > 0. Invalid value: $depth" >&2
            exit 1
        fi

        durations=""
        run=1
        while [ "$run" -le "$STORE_COUNT" ]; do
            db_path="$STORE_DB_ROOT/io_uring_${depth}_run_${run}"
            rm -rf "$db_path"

            cmd_args="store --io-engine io-uring --io-uring-queue-depth $depth -d $db_path $STORE_DATASET"
            if [ -n "$STORE_EXE" ]; then
                output="$($STORE_EXE $cmd_args 2>&1)"
            elif [ -x "./dues" ]; then
                output="$(./dues $cmd_args 2>&1)"
            else
                output="$(go run . $cmd_args 2>&1)"
            fi

            seconds="$(extract_store_seconds "$output")"
            if [ -z "$seconds" ]; then
                echo "Could not parse store duration for depth $depth run $run. Output:" >&2
                printf '%s\n' "$output" >&2
                exit 1
            fi

            echo "io_uring depth $depth run $run/$STORE_COUNT: ${seconds}s"
            durations="$durations $seconds"
            run=$((run + 1))
        done

        median="$(printf '%s\n' $durations | sort -n | awk '{a[NR]=$1} END {if (NR==0) exit 1; if (NR%2==1) print a[(NR+1)/2]; else printf "%.6f", (a[NR/2]+a[NR/2+1])/2}')"
        {
            echo "depth=$depth median_seconds=$median"
        } | tee -a "$SUMMARY_FILE" | tee -a "$FULL_RUN_FILE" > /dev/null

        if [ -z "$best_median" ] || awk -v cur="$median" -v best="$best_median" 'BEGIN {exit !(cur < best)}'; then
            best_median="$median"
            best_depth="$depth"
        fi
    done

    {
        echo "recommended_depth=$best_depth median_seconds=$best_median"
    } | tee -a "$SUMMARY_FILE" | tee -a "$FULL_RUN_FILE" > /dev/null
}

run_bench "service_getchonkmap" "./lib/service" "^BenchmarkGetChonkMap$"
run_bench "store_ingest_path" "./lib/store" "^BenchmarkIngestDataPath$"
run_bench "store_ingest_metadata_path" "./lib/store" "^BenchmarkIngestMetadataPath$"
run_bench "store_ingest_metadata_hotspots" "./lib/store" "^BenchmarkIngestMetadataPathHotspots$"
run_bench "store_revrel_path" "./lib/store" "^BenchmarkProcessRevRel$"
run_bench "store_repair_scan" "./lib/store" "^BenchmarkInspectEvidenceRepairs$"
run_bench "store_restore_path" "./lib/store" "^BenchmarkRestoreDataPath$"

write_revrel_mode_summary "$OUTPUT_DIR/store_ingest_metadata_hotspots.txt"
write_io_uring_depth_summary

echo "Benchmark run complete. Outputs in $OUTPUT_DIR"
echo "Summary: $SUMMARY_FILE"