#!/bin/sh
# Build script for compression product artifacts.
# - CLI binary from ./cmd/compression
# - Optional FFI c-shared/c-archive from ./cmd/ffi

set -e

PRODUCT_NAME="${PRODUCT_NAME:-compression}"
CLI_PACKAGE="${CLI_PACKAGE:-./cmd/compression}"
FFI_PACKAGE="${FFI_PACKAGE:-./cmd/ffi}"
OUTPUT_DIR="${OUTPUT_DIR:-dist/compression}"
TARGETS="${TARGETS:-linux/amd64}"
MODE="${MODE:-all}" # cli | ffi | all
FFI_BUILD_MODE="${FFI_BUILD_MODE:-auto}" # auto | c-shared | c-archive
BUILD_PROFILE="${BUILD_PROFILE:-debug}" # debug | release

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_ROOT"

if ! command -v go > /dev/null 2>&1; then
  echo "ERROR: Go compiler not found in PATH." >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"

GO_LDFLAGS=""
case "$BUILD_PROFILE" in
  debug)
    echo "Build profile: debug"
    ;;
  release)
    echo "Build profile: release (stripped symbols/debug info)"
    GO_LDFLAGS="-s -w"
    ;;
  *)
    echo "ERROR: Invalid BUILD_PROFILE '$BUILD_PROFILE'. Use debug|release." >&2
    exit 1
    ;;
esac

build_cli() {
  _goos="$1"
  _goarch="$2"
  _ext=""
  if [ "$_goos" = "windows" ]; then
    _ext=".exe"
  fi

  _out="$OUTPUT_DIR/${PRODUCT_NAME}-$_goos-$_goarch$_ext"
  echo "[CLI] $_goos/$_goarch -> $_out"

  if [ -n "$GO_LDFLAGS" ]; then
    GOOS="$_goos" GOARCH="$_goarch" GOAMD64=v1 CGO_ENABLED=0 \
      go build -trimpath -buildvcs=false -tags "notusk" -ldflags "$GO_LDFLAGS" -o "$_out" "$CLI_PACKAGE"
  else
    GOOS="$_goos" GOARCH="$_goarch" GOAMD64=v1 CGO_ENABLED=0 \
      go build -trimpath -buildvcs=false -tags "notusk" -o "$_out" "$CLI_PACKAGE"
  fi
}

build_ffi() {
  _goos="$1"
  _goarch="$2"
  _resolved_ffi_build_mode="$FFI_BUILD_MODE"

  if [ "$_resolved_ffi_build_mode" = "auto" ]; then
    if [ "$_goos" = "windows" ]; then
      _resolved_ffi_build_mode="c-shared"
    else
      _resolved_ffi_build_mode="c-archive"
    fi
  fi

  _base="$OUTPUT_DIR/dues_engine-$_goos-$_goarch"
  _ext=".a"

  if [ "$_resolved_ffi_build_mode" = "c-shared" ]; then
    if [ "$_goos" = "windows" ]; then
      _ext=".dll"
    elif [ "$_goos" = "darwin" ]; then
      _ext=".dylib"
    else
      _ext=".so"
    fi
  fi
  _out="${_base}${_ext}"

  echo "[FFI:$_resolved_ffi_build_mode] $_goos/$_goarch -> $_out"
  if [ "$_goos" = "windows" ] && [ "$_resolved_ffi_build_mode" = "c-archive" ]; then
    echo "WARN: windows c-archive output is GNU .a and is typically not linkable by MSVC toolchains." >&2
    echo "WARN: For Flutter on Windows, prefer FFI_BUILD_MODE=c-shared and load the DLL via dart:ffi." >&2
  fi

  if [ "$_resolved_ffi_build_mode" != "c-shared" ] && [ "$_resolved_ffi_build_mode" != "c-archive" ]; then
    echo "ERROR: Invalid FFI_BUILD_MODE '$FFI_BUILD_MODE'. Use auto|c-shared|c-archive." >&2
    exit 1
  fi

  if [ -n "$GO_LDFLAGS" ]; then
    GOOS="$_goos" GOARCH="$_goarch" GOAMD64=v1 CGO_ENABLED=1 \
      go build -trimpath -buildvcs=false -tags "notusk" -ldflags "$GO_LDFLAGS" -buildmode="$_resolved_ffi_build_mode" -o "$_out" "$FFI_PACKAGE"
  else
    GOOS="$_goos" GOARCH="$_goarch" GOAMD64=v1 CGO_ENABLED=1 \
      go build -trimpath -buildvcs=false -tags "notusk" -buildmode="$_resolved_ffi_build_mode" -o "$_out" "$FFI_PACKAGE"
  fi
  # Go also emits ${_base}.h for c-shared/c-archive.
}

OLD_IFS="$IFS"
IFS=','
for target in $TARGETS; do
  IFS="$OLD_IFS"
  goos="${target%/*}"
  goarch="${target#*/}"

  if [ -z "$goos" ] || [ -z "$goarch" ] || [ "$goos" = "$goarch" ]; then
    echo "ERROR: Invalid target '$target'. Expected os/arch." >&2
    exit 1
  fi

  case "$MODE" in
    cli)
      build_cli "$goos" "$goarch"
      ;;
    ffi)
      build_ffi "$goos" "$goarch"
      ;;
    all)
      build_cli "$goos" "$goarch"
      build_ffi "$goos" "$goarch"
      ;;
    *)
      echo "ERROR: Invalid MODE '$MODE'. Use cli|ffi|all." >&2
      exit 1
      ;;
  esac

  IFS=','
done
IFS="$OLD_IFS"

echo "Build complete -> $OUTPUT_DIR"
