#!/bin/sh
# Builds dues for every supported platform. The binary is pure Go (no CGO, no
# libtusk), so a single host cross-compiles the whole matrix with no C toolchain.
#
# Override the target list with the TARGETS env var (space-separated os/arch),
# e.g. TARGETS="linux/amd64 darwin/arm64" ./build.sh

set -e

BINARY_NAME="${BINARY_NAME:-dues}"
MAIN_PACKAGE="${MAIN_PACKAGE:-.}"
OUTPUT_DIR="${OUTPUT_DIR:-dist}"
TARGETS="${TARGETS:-windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

# ANSI colour helpers
C_RESET="\033[0m"
C_DARKGRAY="\033[90m"
C_WHITE="\033[97m"
C_CYAN="\033[96m"
C_YELLOW="\033[93m"
C_GREEN="\033[92m"
C_RED="\033[91m"
C_DARKYELLOW="\033[33m"
C_DARKCYAN="\033[36m"

line="========================================================================"

# banner <title> [title_color]
banner() {
    _title="$1"
    _color="${2:-$C_CYAN}"
    printf "\n${C_DARKGRAY}%s${C_RESET}\n  ${_color}%s${C_RESET}\n${C_DARKGRAY}%s${C_RESET}\n" \
        "$line" "$_title" "$line"
}

# label <key> <value> [value_color]
label() {
    _vc="${3:-$C_WHITE}"
    printf "${C_DARKGRAY}%-16s${C_RESET}${_vc}%s${C_RESET}\n" "$1" "$2"
}

if ! command -v go > /dev/null 2>&1; then
    printf "${C_YELLOW}ERROR: Go compiler not found in PATH. Install Go and retry.${C_RESET}\n" >&2
    exit 1
fi

if [ -d "$OUTPUT_DIR" ]; then
    rm -rf "$OUTPUT_DIR"
fi
mkdir -p "$OUTPUT_DIR"

LDFLAGS="-s -w -buildid="

PGO_FILE="$REPO_ROOT/default.pgo"
if [ -f "$PGO_FILE" ]; then
    PGO_STATUS="Enabled ($PGO_FILE)"
    PGO_COLOR="$C_GREEN"
else
    PGO_FILE=""
    PGO_STATUS="Not found (building without -pgo)"
    PGO_COLOR="$C_DARKYELLOW"
fi

# Count targets.
TARGET_COUNT=0
for _t in $TARGETS; do
    TARGET_COUNT=$((TARGET_COUNT + 1))
done

banner "DUES Release Build"
label "Repository" "$REPO_ROOT" "$C_CYAN"
label "Output Dir" "$REPO_ROOT/$OUTPUT_DIR" "$C_CYAN"
label "Targets" "$TARGETS" "$C_YELLOW"
label "Mode" "Pure-Go static release build (CGO=0)" "$C_GREEN"
label "PGO" "$PGO_STATUS" "$PGO_COLOR"

BUILD_START=$(date +%s)
INDEX=0

for target in $TARGETS; do
    INDEX=$((INDEX + 1))
    GOOS="${target%/*}"
    GOARCH="${target#*/}"
    if [ -z "$GOOS" ] || [ -z "$GOARCH" ] || [ "$GOOS" = "$target" ]; then
        printf "${C_RED}ERROR: invalid target '%s' (expected os/arch)${C_RESET}\n" "$target" >&2
        exit 1
    fi

    EXT=""
    if [ "$GOOS" = "windows" ]; then
        EXT=".exe"
    fi

    OUTPUT_NAME="${BINARY_NAME}-${GOOS}-${GOARCH}${EXT}"
    OUTPUT_PATH="${OUTPUT_DIR}/${OUTPUT_NAME}"

    printf "\n${C_DARKGRAY}[%d/%d] ${C_RESET}${C_YELLOW}%s${C_RESET}  ${C_DARKCYAN}(CGO=0 pure Go, trimpath, stripped)${C_RESET}\n" \
        "$INDEX" "$TARGET_COUNT" "$OUTPUT_NAME"

    STEP_START=$(date +%s)

    # GOAMD64=v1 is honoured only for amd64 and ignored for other arches, so it is
    # safe to pass unconditionally.
    export GOOS GOARCH CGO_ENABLED=0 GOAMD64=v1
    if [ -n "$PGO_FILE" ]; then
        go build -trimpath -buildvcs=false -tags netgo,osusergo -ldflags "$LDFLAGS" -pgo "$PGO_FILE" -o "$OUTPUT_PATH" "$MAIN_PACKAGE"
    else
        go build -trimpath -buildvcs=false -tags netgo,osusergo -ldflags "$LDFLAGS" -o "$OUTPUT_PATH" "$MAIN_PACKAGE"
    fi

    STEP_END=$(date +%s)
    DURATION=$((STEP_END - STEP_START))

    SIZE_BYTES=$(wc -c < "$OUTPUT_PATH")
    SIZE_MB=$(awk "BEGIN { printf \"%.2f\", $SIZE_BYTES / 1048576 }")

    printf "  ${C_GREEN}OK${C_RESET}  ${C_CYAN}%s MB${C_RESET}  ${C_DARKGRAY}%ds${C_RESET}\n" "$SIZE_MB" "$DURATION"
done

BUILD_END=$(date +%s)
BUILD_SECONDS=$((BUILD_END - BUILD_START))

banner "Build Complete" "$C_GREEN"
label "Duration" "${BUILD_SECONDS}s" "$C_GREEN"
label "Artifacts" "$TARGET_COUNT" "$C_GREEN"
printf "\n${C_DARKGRAY}%-32s %s${C_RESET}\n" "Binary" "Size(MB)"
for f in "$OUTPUT_DIR"/${BINARY_NAME}-*; do
    [ -f "$f" ] || continue
    _sz=$(awk "BEGIN { printf \"%.2f\", $(wc -c < "$f") / 1048576 }")
    printf "${C_WHITE}%-32s %s${C_RESET}\n" "$(basename "$f")" "$_sz"
done
printf "\n"
