#!/usr/bin/env bash
# Linux-only helper to build compression product static FFI library (.a).
# Intended to run inside Linux/WSL.
#
#   ./build-compression-linux.sh [--targets OS/ARCH,...] [--output-dir DIR] [--build-profile debug|release]

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

TARGETS=linux/amd64
OUTPUT_DIR=dist/compression
BUILD_PROFILE=release
while [[ $# -gt 0 ]]; do
  case "$1" in
    --targets | --output-dir | --build-profile)
      [[ $# -ge 2 ]] || { echo "ERROR: $1 needs a value" >&2; exit 2; }
      case "$1" in
        --targets) TARGETS="$2" ;;
        --output-dir) OUTPUT_DIR="$2" ;;
        --build-profile) BUILD_PROFILE="$2" ;;
      esac
      shift 2
      ;;
    *) echo "ERROR: unknown argument '$1'" >&2; exit 2 ;;
  esac
done

./build-compression.sh --mode ffi --ffi-build-mode c-archive \
  --targets "$TARGETS" --output-dir "$OUTPUT_DIR" --build-profile "$BUILD_PROFILE"

echo "Linux static FFI build complete."
echo "Artifacts expected in: $OUTPUT_DIR"
