# Toolchain Notes: CGO, Static Linking, Musl, Zig, and MinGW

This document captures the toolchain decisions and constraints around building
`dues` with CGO for the `windows/amd64` and `linux/amd64` targets. Both require
linking `libtusk`, a C++ static archive. Everything here is a result of working
through real build failures.

---

## Why CGO Is Required for Some Targets

`libtusk` exposes a C API (`libtusk_analyze`, `libtusk_free`) wrapping The
Sleuth Kit (TSK), a large C/C++ forensics library. Go's FFI mechanism, CGO, is
the only first-class way to call into a static archive at compile time.

CGO is only enabled for `windows/amd64` and `linux/amd64`. All other targets
(`linux/arm64`, `darwin/*`, `windows/arm64`) build with `CGO_ENABLED=0` because
no matching pre-built `libtusk` archive exists for those platforms.

---

## The Core Constraint: ABI Compatibility

This is the single most important rule:

> **The C++ static archive (`libtusk_*.a`) and the final binary must be built
> with toolchains that share the same C++ ABI and C runtime.**

A static archive is not self-contained. It carries unresolved external
references to symbols from the C++ standard library and C runtime. When the
linker combines the archive with Go's object files, those external symbols must
exist in whatever runtime the linker is targeting.

If the archive was compiled with toolchain A and the linker targets toolchain B,
you get `undefined symbol` errors for things like
`std::string::~basic_string()`, `__snprintf_chk`, vtables, etc. — exactly what
happened with zig+musl vs a glibc-compiled archive.

---

## Linux/amd64: Musl vs Glibc

### Why Static Linking on Linux Is Hard

Linux binaries conventionally link against **glibc** (the GNU C Library). Glibc
is designed for dynamic linking. While glibc allows `-static`, it has
well-documented limitations:

- `getaddrinfo` (DNS) uses `dlopen` internally to load NSS modules at runtime. A
  statically linked glibc binary doing DNS resolution can fail or crash on
  systems with a different glibc version because it tries to `dlopen` a `.so`
  that may not exist or may be ABI-incompatible.
- Glibc's `_FORTIFY_SOURCE` injects checked variants of standard functions
  (`__snprintf_chk`, `__fprintf_chk`, `__vsnprintf_chk`, `__vfprintf_chk`).
  These are glibc-internal symbols. If your archive was compiled with
  `_FORTIFY_SOURCE` (which is common in release builds on Ubuntu/Debian with
  `-O2`) and your linker targets musl or a different glibc, the symbols are
  simply absent.

### Why Musl Solves This

**musl-libc** is designed from the ground up for clean static linking. A binary
linked against musl statically is a single self-contained ELF with no runtime
dependencies. It runs on any Linux kernel regardless of what userspace is
installed.

The build script uses `x86_64-linux-musl-gcc` (or the zig wrapper) to target
musl. The resulting binary has zero shared library dependencies.

### Zig as a Cross-Compiler

`zig cc` bundles a full cross-compilation toolchain including:

- LLVM/clang compiler
- Musl libc headers and static libraries
- `compiler-rt`

It can target any supported triple directly from Windows:

```
zig cc -target x86_64-linux-musl
```

Because CMake and Go's CGO expect `CC` to be a plain command (no embedded
spaces), a `.cmd` wrapper is used:

**`zigcc-linux-amd64.cmd`** (in repo root):

```bat
@zig cc -target x86_64-linux-musl %*
```

Set as `CC` before `go build`:

```
CC=zigcc-linux-amd64.cmd CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build ...
```

### The ABI Mismatch We Hit

The current `libtusk_lnx.a` was compiled on a Linux machine with:

- **System GCC** (gcc-13)
- **Glibc headers** (`/usr/include/x86_64-linux-gnu/`)
- **libstdc++** (GCC's C++ standard library)

The final link step with `zig cc -target x86_64-linux-musl` failed because:

1. Musl does not have `__snprintf_chk` / `__fprintf_chk` / etc. — these are
   glibc `_FORTIFY_SOURCE` wrapper symbols compiled into the object files.
2. Zig uses LLVM's **libc++**, not **libstdc++**. The two C++ ABIs are
   incompatible. Symbols like `std::__cxx11::basic_string<...>::~basic_string()`
   live in libstdc++; libc++ uses different symbol names and a different vtable
   layout.

**Fix:** `libtusk_lnx.a` must be rebuilt from the tusk source using
`zig cc -target x86_64-linux-musl` as the C/C++ compiler. Both the archive and
the final binary must target the same ABI.

Rebuilding with zig (on any machine with zig installed):

```bash
# zigcc.sh / zigcxx.sh wrappers:
#   zigcc.sh:  zig cc  -target x86_64-linux-musl "$@"
#   zigcxx.sh: zig c++ -target x86_64-linux-musl "$@"

cd /path/to/tusk
cmake -S . -B build \
  -DCMAKE_C_COMPILER=/path/to/zigcc.sh \
  -DCMAKE_CXX_COMPILER=/path/to/zigcxx.sh \
  -DCMAKE_BUILD_TYPE=Release
cmake --build build
# copy build/libtusk.a -> clib/libtusk_lnx.a
```

---

## Windows/amd64: MinGW vs MSVC

### Why MSVC Cannot Be Used

Go's CGO toolchain on Windows requires a GCC-compatible compiler. The CGO bridge
generates C code that is compiled with whatever `CC` points to. MSVC (`cl.exe`):

- Uses a completely different command-line interface (`/Fe`, `/Fo`, `/MT`, etc.)
  instead of GCC-style flags (`-o`, `-c`, `-static`)
- Uses its own C++ ABI (MSVC ABI) incompatible with GCC's Itanium ABI
- Cannot be driven by CGO's internal compiler invocation

MSVC is therefore **not supported** for CGO builds of Go programs.

### MinGW-w64

**MinGW-w64** provides GCC targeting Windows PE/COFF executables. It is the
standard CGO toolchain on Windows.

The build uses `x86_64-w64-mingw32-gcc` as the cross-compiler for
`windows/amd64`. This must be a proper MinGW-w64 cross-compiler targeting
`x86_64-w64-mingw32`. On Windows, this is available from:

- **MSYS2**: `pacman -S mingw-w64-x86_64-gcc` (recommended, most up-to-date)
- **Scoop** (`extras` bucket): `scoop install mingw`
- **WinLibs** (standalone distribution): https://winlibs.com

The regular `gcc` in PATH on Windows is almost always the MinGW compiler that
comes with Git for Windows or MSYS2. It targets the host Windows machine
(`x86_64-pc-windows-gnu`), which is fine when building for `windows/amd64`
natively. However, the build script runs on Windows and cross-compiles all
targets, so the `x86_64-w64-mingw32-gcc` form is used explicitly to avoid
ambiguity.

### Why -static Works on Windows With MinGW

MinGW uses `msvcrt.dll` (the old MSVC C runtime) or `ucrt.dll` as its C runtime,
but all of MinGW's own runtime pieces (`libgcc`, `libstdc++`, `libmingw32`) can
be statically linked:

```
-static -lstdc++ -lm
```

This embeds the C++ runtime and math library into the binary. The final binary
still depends on `kernel32.dll`, `ntdll.dll`, etc. (Windows kernel DLLs), but
those are always present and stable across all supported Windows versions.

The glibc `_FORTIFY_SOURCE` problem does not apply on Windows because MinGW does
not inject those checked-function wrappers.

### libtusk_win.a ABI

`libtusk_win.a` was compiled on Linux using `x86_64-w64-mingw32-g++` (a Linux-
hosted MinGW cross-compiler). This produces PE/COFF object files using the MinGW
ABI — the same ABI that `x86_64-w64-mingw32-gcc` on Windows expects.

As long as both sides use MinGW-w64 with the same target triple, the archive
links cleanly. The same ABI-mismatch risk exists here: if the archive were ever
rebuilt with MSVC or with a different ABI (e.g., LLVM/clang-cl targeting MSVC),
the link would fail with C++ symbol errors.

---

## Summary Table

| Target        | CGO | CC                            | C++ Runtime  | C Runtime | libtusk built with                  |
| ------------- | --- | ----------------------------- | ------------ | --------- | ----------------------------------- |
| windows/amd64 | Yes | `x86_64-w64-mingw32-gcc`      | libstdc++    | msvcrt    | `x86_64-w64-mingw32-g++`            |
| linux/amd64   | Yes | `zigcc-linux-amd64.cmd` (zig) | libc++ (zig) | musl      | `zig c++ -target x86_64-linux-musl` |
| linux/arm64   | No  | —                             | —            | —         | not needed                          |
| darwin/amd64  | No  | —                             | —            | —         | not needed                          |
| darwin/arm64  | No  | —                             | —            | —         | not needed                          |
| windows/arm64 | No  | —                             | —            | —         | not needed                          |

---

## Checklist When Regenerating libtusk Archives

### libtusk_lnx.a

- [ ] Build with `zig c++ -target x86_64-linux-musl` (not system GCC)
- [ ] Do not compile with `-D_FORTIFY_SOURCE` (or ensure zig suppresses it)
- [ ] Do not link against system libstdc++; zig bundles its own libc++
- [ ] Verify with `nm libtusk_lnx.a | grep __snprintf_chk` — must return nothing
- [ ] Verify with `nm libtusk_lnx.a | grep musl` or check for musl-linked
      symbols

### libtusk_win.a

- [ ] Build with `x86_64-w64-mingw32-g++` (MinGW-w64, not MSVC, not clang-cl)
- [ ] Verify PE/COFF format: `file libtusk_win.a` should show `Windows` object
      files
- [ ] Ensure `-lstdc++ -lm` is present in the Go `#cgo windows LDFLAGS`
      directive
