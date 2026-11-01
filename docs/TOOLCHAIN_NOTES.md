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
  uncommitted changes. The banner shows it, and the identity line
  (`dues --version`, and `dues version` on a pipe) carries it as semver build
  metadata: `0.39.0+74431db`. A tree without git builds without it.

The `netgo` and `osusergo` tags select Go's own DNS resolver and user lookup.

| | `build.ps1` | `build.sh` |
|---|---|---|
| Output | `dist\dues-<os>-<arch>[.exe]` | `dist/dues-<os>-<arch>[.exe]` |
| Choose targets | `-Targets linux/amd64,darwin/arm64` | `--targets "linux/amd64 darwin/arm64"` |
| Other settings | `-BinaryName`, `-MainPackage`, `-OutputDir` | `--binary-name`, `--main-package`, `--output-dir` |
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
| `faultinject` | `internal/faultinject`, `internal/e2e`, `lib/store` tests, root and `cmd/compression` | Turns on named failure points, and the hidden `--faults` and `--faults-notify` flags. The verify gate fails a release binary that carries this tag. |
| `safestrict` | `internal/safe` | A panic `internal/safe` would contain crashes instead; the fuzz smoke builds with it. The verify gate fails a release binary that carries it. |
| `long` | `internal/testutil` | Runs the long tests. |
| `requirepty` | `internal/e2e` tests | The pseudo-terminal tests fail rather than skip. |
| `winconsole` | `internal/faultinject` tests | Runs the Ctrl-Break test, which needs a Windows console. |
| `fixturegen` | `lib/graph/fixturegen_test.go` | Regenerates a legacy graph fixture. Read the file's header before you run it. |

The rest are the standard `windows` and `linux` platform constraints.

## Environment variables

DUES reads none: every setting is a flag (or, for the FFI library, its
`DuesConfigure` document), and `internal/osdirs` finds the default
directories from the operating system. `internal/osdirs/noenv_guard_test.go`
fails on any environment read in DUES's code, tests included.
`internal/e2e/environment_test.go` checks that a run prints the same with
the old variables set.

The build scripts set Go's own variables (`GOOS`, `GOARCH`, `CGO_ENABLED`,
`GOAMD64`, `CC`, `GOTOOLCHAIN`), the toolchain's interface, and
`scripts/fixtures/gen_legacy_dbs.sh` sets `MSYS_NO_PATHCONV` for Git Bash.

Some dependencies read the environment inside their own code, which DUES
does not change. As of this writing (checked in the module sources):

| Dependency | Variables | What they change, for DUES |
|---|---|---|
| `github.com/charmbracelet/x/ansi` | `RUNEWIDTH_EASTASIAN` | At start-up, `1` makes East Asian ambiguous-width characters two columns wide. DUES measures and cuts its live display with this package, so on a terminal the rows may be cut at other columns. Pipes and `--plain` are not affected. The setting is not exported, so DUES cannot undo it. |
| `github.com/mattn/go-runewidth` | `RUNEWIDTH_EASTASIAN`, the locale (`LC_ALL`, `LC_CTYPE`, `LANG`), `WT_SESSION` on Windows | Its package defaults, which DUES does not use; `x/ansi` passes its own options. |
| `charm.land/lipgloss/v2` | the colour variables, at start-up | Only its `Print` functions, which DUES never calls (a guard test bans them). |
| `charm.land/bubbletea/v2`, `github.com/charmbracelet/ultraviolet` | `TEA_DEBUG`, `TEA_TRACE`, the terminal variables | The TUI only. |
| `github.com/xo/terminfo` | `TERM`, `TERMINFO`, `COLORTERM` and the like | Only when called; DUES does not call it. |
| `google.golang.org/grpc` | `GRPC_GO_*` and xDS variables | Logging and xDS features of the gRPC library. |
| `google.golang.org/protobuf` | `GOPROTODEBUG`, the registry conflict policy | Debug behaviour. |
| `golang.org/x/net/http2`, `golang.org/x/sys/cpu` | `GODEBUG`, `DEBUG_HTTP2_GOROUTINES` | Debugging and CPU feature detection. |
| `github.com/zeebo/blake3` | `BLAKE3_PUREGO`, `BLAKE3_DISABLE_AVX2`, `BLAKE3_DISABLE_SSE41` | Which implementation runs; the hash is the same. |
| `go.etcd.io/bbolt` | its verification switch | Extra checks. |
| `github.com/shirou/gopsutil/v3` | `HOST_PROC`, `HOST_SYS` and the like | Where it reads system information. |
| `go.opentelemetry.io/otel`, `go.opentelemetry.io/auto/sdk` | span limits | Tracing, which DUES does not export. |
| `www.velocidex.com/golang/evtx` (Windows) | `SystemRoot`, `WinDir`, `ProgramFiles` | Where it looks for message files. |
| `www.velocidex.com/golang/go-pe`, `regparser` | their debug switches | Debug output. |
| `github.com/alecthomas/kingpin/v2` | a flag's `Envar` | Nothing: DUES gives no flag one, and the guard bans it. |

The Go runtime reads `GODEBUG`, `GOMAXPROCS`, `GOTRACEBACK`, `GOGC`,
`GOMEMLIMIT` and `TZ`, and `os.Getwd` uses `PWD` after checking it names the
working directory.

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
