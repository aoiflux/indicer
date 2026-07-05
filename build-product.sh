#!/bin/sh
# Product build wrapper script.
# Routes to product-specific build scripts.

set -e

PRODUCT="${PRODUCT:-compression}"
MODE="${MODE:-all}"
TARGETS="${TARGETS:-linux/amd64}"
OUTPUT_DIR="${OUTPUT_DIR:-}"
FFI_BUILD_MODE="${FFI_BUILD_MODE:-auto}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --product)
      PRODUCT="$2"
      shift 2
      ;;
    --mode)
      MODE="$2"
      shift 2
      ;;
    --targets)
      TARGETS="$2"
      shift 2
      ;;
    --output-dir)
      OUTPUT_DIR="$2"
      shift 2
      ;;
    --ffi-build-mode)
      FFI_BUILD_MODE="$2"
      shift 2
      ;;
    *)
      echo "ERROR: Unknown argument '$1'" >&2
      exit 1
      ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

case "$PRODUCT" in
  compression)
    SCRIPT="$REPO_ROOT/build-compression.sh"
    ;;
  *)
    echo "ERROR: Unsupported product '$PRODUCT'" >&2
    exit 1
    ;;
esac

if [ ! -f "$SCRIPT" ]; then
  echo "ERROR: Missing script '$SCRIPT'" >&2
  exit 1
fi

if [ -n "$OUTPUT_DIR" ]; then
  PRODUCT_NAME="$PRODUCT" MODE="$MODE" TARGETS="$TARGETS" FFI_BUILD_MODE="$FFI_BUILD_MODE" OUTPUT_DIR="$OUTPUT_DIR" sh "$SCRIPT"
else
  PRODUCT_NAME="$PRODUCT" MODE="$MODE" TARGETS="$TARGETS" FFI_BUILD_MODE="$FFI_BUILD_MODE" sh "$SCRIPT"
fi
