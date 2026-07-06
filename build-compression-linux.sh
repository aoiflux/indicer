#!/usr/bin/env bash
# Linux-only helper to build compression product static FFI library (.a).
# Intended to run inside Linux/WSL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "ERROR: This script must run on Linux/WSL." >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: Go compiler not found in PATH." >&2
  exit 1
fi

TARGETS="${TARGETS:-linux/amd64}"
OUTPUT_DIR="${OUTPUT_DIR:-dist/compression}"
BUILD_PROFILE="${BUILD_PROFILE:-release}"

MODE=ffi \
FFI_BUILD_MODE=c-archive \
TARGETS="$TARGETS" \
OUTPUT_DIR="$OUTPUT_DIR" \
BUILD_PROFILE="$BUILD_PROFILE" \
./build-compression.sh

echo "Linux static FFI build complete."
echo "Artifacts expected in: $OUTPUT_DIR"
