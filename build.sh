#!/bin/sh
# Builds dues for Linux/amd64.
# To compile for Windows, run build.ps1 on Windows using PowerShell.

set -e

BINARY_NAME="${BINARY_NAME:-dues}"
MAIN_PACKAGE="${MAIN_PACKAGE:-.}"
OUTPUT_DIR="${OUTPUT_DIR:-dist}"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

# ANSI colour helpers
C_RESET="\033[0m"
C_DARKGRAY="\033[90m"
C_WHITE="\033[97m"
C_CYAN="\033[96m"
C_YELLOW="\033[93m"
C_GREEN="\033[92m"
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
    PGO_ARG=1
    PGO_STATUS="Enabled ($PGO_FILE)"
    PGO_COLOR="$C_GREEN"
else
    PGO_ARG=""
    PGO_STATUS="Not found (building without -pgo)"
    PGO_COLOR="$C_DARKYELLOW"
fi

banner "DUES Release Build"
label "Repository" "$REPO_ROOT" "$C_CYAN"
label "Output Dir" "$REPO_ROOT/$OUTPUT_DIR" "$C_CYAN"
label "Target" "linux/amd64" "$C_YELLOW"
label "Mode" "Optimized static-style release build" "$C_GREEN"
label "PGO" "$PGO_STATUS" "$PGO_COLOR"

OUTPUT_NAME="${BINARY_NAME}-linux-amd64"
OUTPUT_PATH="${OUTPUT_DIR}/${OUTPUT_NAME}"
CGO_LABEL="CGO=0 (pure Go)"

printf "\n${C_DARKGRAY}[1/1] ${C_RESET}${C_YELLOW}%s${C_RESET}  ${C_DARKCYAN}(%s, trimpath, stripped)${C_RESET}\n" "$OUTPUT_NAME" "$CGO_LABEL"

BUILD_START=$(date +%s)

GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 \
    go build \
        -trimpath \
        -buildvcs=false \
        -tags netgo,osusergo \
        -ldflags "$LDFLAGS" \
        ${PGO_ARG:+-pgo "$PGO_FILE"} \
        -o "$OUTPUT_PATH" \
        "$MAIN_PACKAGE"

BUILD_END=$(date +%s)
DURATION=$((BUILD_END - BUILD_START))

SIZE_BYTES=$(wc -c < "$OUTPUT_PATH")
SIZE_KB=$((SIZE_BYTES / 1024))

printf "  ${C_GREEN}OK${C_RESET}  ${C_CYAN}%d KB${C_RESET}  ${C_DARKGRAY}%ds${C_RESET}\n" "$SIZE_KB" "$DURATION"

banner "Build Complete" "$C_GREEN"
label "Duration" "${DURATION}s" "$C_GREEN"
label "Artifacts" "1" "$C_GREEN"
printf "\n${C_DARKGRAY}%-40s %s${C_RESET}\n" "Binary" "Size(KB)"
printf "${C_WHITE}%-40s %s${C_RESET}\n" "$OUTPUT_NAME" "$SIZE_KB"
printf "\n"

printf "${C_DARKYELLOW}NOTE: This script builds for Linux only. To compile for Windows, run build.ps1 on Windows using PowerShell.${C_RESET}\n"
