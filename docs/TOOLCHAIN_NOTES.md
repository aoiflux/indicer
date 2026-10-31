# Toolchain notes

This page lists the Go versions, analysis tools, build scripts and C toolchain
needs of DUES. For the verify gate, see [TESTING.md](TESTING.md).

## Go versions and tools

| What | Value | Set in | Effect |
|---|---|---|---|
| Language version | `go 1.26.2` | `go.mod` | The oldest Go the module accepts |
| Toolchain | `go1.27.3` | `go.mod` | With the default `GOTOOLCHAIN=auto`, an older local `go` downloads and runs `go1.27.3` |
| Analysis toolchain | `go1.26.6` | `scripts/tools.env` (`ANALYSIS_TOOLCHAIN`) | The verify scripts run staticcheck with `GOTOOLCHAIN=go1.26.6` |
| staticcheck | `honnef.co/go/tools/cmd/staticcheck@v0.8.1` | `scripts/tools.env` (`STATICCHECK`) | Run with `go run`, once without and once with `-tags faultinject` |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck@v1.8.0` | `scripts/tools.env` (`GOVULNCHECK`) | Run with `go run` in source mode, and in binary mode on a linux/amd64 build |

- `scripts/verify.ps1` and `scripts/verify.sh` both read `scripts/tools.env`.
  Change tool versions there only.
- staticcheck runs under its own pinned toolchain because older staticcheck
  releases cannot read the export data that newer toolchains write.
- `GOTOOLCHAIN=go1.26.6` overrides the `toolchain` line in `go.mod`. It works
  because `go1.26.6` is not older than the `go 1.26.2` line.

## Building `dues`

`dues` is pure Go. It needs no cgo and no C compiler on any target.

`build.ps1` and `build.sh` build six targets from one host: `windows`, `linux`
and `darwin`, each on `amd64` and `arm64`. Every build uses:

- `CGO_ENABLED=0`;
- `GOAMD64=v1` (amd64 only; `build.sh` sets it for every target, and other
  architectures ignore it);
- `go build -trimpath -buildvcs=false -tags netgo,osusergo -ldflags "-s -w -buildid="`;
- `-pgo default.pgo` when the repository root has a `default.pgo` (none is
  committed);
- `-X main.buildCommit=<short commit>`, with `-dirty` when the tree has
  uncommitted changes. `dues version` and the banner show it. A tree without
  git builds without it.

The `netgo` and `osusergo` tags select Go's own DNS resolver and user lookup.

| | `build.ps1` | `build.sh` |
|---|---|---|
| Output | `dist\dues-<os>-<arch>[.exe]` | `dist/dues-<os>-<arch>[.exe]` |
| Choose targets | `-Targets linux/amd64,darwin/arm64` | `TARGETS="linux/amd64 darwin/arm64"` |
| Other settings | `-BinaryName`, `-MainPackage`, `-OutputDir` | `BINARY_NAME`, `MAIN_PACKAGE`, `OUTPUT_DIR` |
| Old output | Deleted first, unless `-NoClean` | Always deleted first |
| Checksums | `dist\SHA256SUMS` | `dist/SHA256SUMS` |

Both scripts end by writing `SHA256SUMS` beside the binaries, to publish with
them. It has one line for each `dues-*` file in the output directory: the
lowercase SHA-256, two spaces, the file name. Lines end in LF. With `-NoClean`
it also covers binaries left there by an earlier run. `build.sh` needs
`sha256sum` or `shasum` and checks for them before it builds.

The builds are reproducible (`-trimpath`, `-buildid=`), so with the same Go
toolchain and the same commit `build.ps1`, `build.sh` and `build.sh` under WSL
write the same sums.

To check a download, run one of these in the folder that holds the binary and
`SHA256SUMS`. Each prints `OK` for a match:

```sh
grep ' dues-linux-amd64$' SHA256SUMS | sha256sum -c          # Linux
grep ' dues-darwin-arm64$' SHA256SUMS | shasum -a 256 -c     # macOS
```

```powershell
(Get-FileHash .\dues-windows-amd64.exe -Algorithm SHA256).Hash.ToLower()
```

On Windows, compare the printed hash with the file's line in `SHA256SUMS`.

`build.ps1` also clears `CC` for the build and restores `CC`, `CGO_ENABLED`,
`GOOS`, `GOARCH` and `GOAMD64` afterwards.

The verify gate cross-builds `.` and `./cmd/compression` for the same six
targets with `CGO_ENABLED=0 -trimpath -tags netgo,osusergo`.

## Build tags

No build tag chooses a parser or a C dependency. The tags in the tree are:

| Tag | Used by | Purpose |
|---|---|---|
| `faultinject` | `internal/faultinject`, `internal/e2e`, `lib/store` tests | Turns on named failure points for tests. The verify gate fails a release binary that carries this tag. |
| `fixturegen` | `lib/graph/fixturegen_test.go` | Regenerates a legacy graph fixture. Read the file's header before you run it. |

The rest are the standard `windows` and `linux` platform constraints.

## Evidence parsing

Parsing is pure Go, in `lib/parser`:

| Layer | Package | Libraries |
|---|---|---|
| Images | `lib/parser/image` | `github.com/aoiflux/libewf`, `github.com/aoiflux/libvhdi` |
| Partition tables | `lib/parser/partition` | `github.com/aoiflux/libtable`, `github.com/diskfs/go-diskfs` |
| File systems | `lib/parser/fs/<name>` | `github.com/aoiflux/libntfs`, `libext`, `libfat`, `libxfat`, `libhfs`, `libxfs` |

During ingest, `lib/evidence` opens images and `lib/index` reads partitions
and file systems (through `lib/parser/scan`). `lib/parser/tskcompat` and
`lib/parser/pdiff` are comparison tools from the move away from libtsk; no
production code imports them.

## Compression product and FFI

The FFI library is the only part of the repository that uses cgo
(`cmd/ffi/main.go`).

`build-compression.ps1` and `build-compression.sh` build the compression
product. `build-product.ps1` and `build-product.sh` route to them.

| Setting | `build-compression.ps1` | `build-compression.sh` | Default |
|---|---|---|---|
| What to build | `-Mode cli\|ffi\|all` | `MODE` | `all` |
| Library kind | `-FfiBuildMode auto\|c-shared\|c-archive` | `FFI_BUILD_MODE` | `auto` |
| Build profile | `-BuildProfile debug\|release` | `BUILD_PROFILE` | `debug` |
| Targets | `-Targets` | `TARGETS`, separated by commas | `windows/amd64` (PowerShell), `linux/amd64` (sh) |
| Output | `-OutputDir` | `OUTPUT_DIR` | `dist/compression` |

- The CLI builds from `./cmd/compression` with `CGO_ENABLED=0`.
- The library builds from `./cmd/ffi` with `CGO_ENABLED=1` and
  `-buildmode c-shared` or `c-archive`. `auto` picks `c-shared` (`.dll`) on
  Windows and `c-archive` (`.a`) elsewhere. Go also writes a C header beside it.
- `release` adds `-ldflags "-s -w"`.
- `build-product.ps1` has no build-profile parameter, so it always builds
  `debug`.
- `build-compression-linux.sh` runs only on Linux or WSL. It builds the library
  as a `c-archive` with the `release` profile.

The library build needs a C compiler for the target:

- The scripts do not set `CC`, so cgo uses the compiler that `go env CC`
  names.
- On Windows, cgo needs a GCC-compatible compiler such as MinGW-w64 `gcc` on
  `PATH`. MSVC `cl.exe` does not work with cgo.
- A `c-archive` built on Windows is a GNU `.a`, which MSVC toolchains do not
  link. For Flutter on Windows, use `c-shared`.
- Building the library for another OS needs a cross C compiler. Build the Linux
  library inside Linux or WSL instead.

[COMPRESSION_ABI_REFERENCE.md](COMPRESSION_ABI_REFERENCE.md) describes the
outputs and the library's ABI.

## History

Until commit `2991008` (2026-08-11), `dues` linked `libtusk`, a C++ wrapper of
The Sleuth Kit, through cgo on `windows/amd64` and `linux/amd64`. That needed
MinGW-w64 on Windows and a zig/musl toolchain on Linux, with matching C++ ABIs.
The `notusk` build tag, the `--parser` flag, `lib/parser/tusk_amd64.go`,
`lib/parser/tusk_fallback.go` and `clib/` went with it. The old notes on those
toolchains no longer apply. To read them:

```sh
git show 2991008:docs/TOOLCHAIN_NOTES.md
```
