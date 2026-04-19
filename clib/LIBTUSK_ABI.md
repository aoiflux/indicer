# LibTusk API Specification

`libtusk` exposes a single public C API in two build forms:

- `build/libtusk.a`: static archive for link-time integration
- `build/libtusk.so` and `build/libtusk.so.1`: shared library for runtime linking or loading

The supported public interface is defined by `include/libtusk.h`.

## Public API

These are the only supported public functions:

```c
char *libtusk_analyze(const char *image_path);
void libtusk_free(char *ptr);
```

### `libtusk_analyze`

```c
char *libtusk_analyze(const char *image_path);
```

- Input: path to a disk image file
- Output: NUL-terminated JSON string allocated by `libtusk`
- Failure: returns `NULL`
- Ownership: the caller must release the result with `libtusk_free`

### `libtusk_free`

```c
void libtusk_free(char *ptr);
```

- Input: pointer returned by `libtusk_analyze`
- Output: none
- Requirement: do not free the returned buffer with `free`, `C.free`, or a language runtime allocator

## Static vs Shared ABI

### Static Archive: `build/libtusk.a`

- `libtusk.a` is a static archive, not a runtime-loaded module.
- It currently contains one object file, `libtusk.cpp.o`.
- That object file provides the externally linkable definitions of `libtusk_analyze` and `libtusk_free`.
- Any extra archive symbols from C++ or the toolchain are implementation details and are not public API.

### Shared Library: `build/libtusk.so`, `build/libtusk.so.1`

- The shared library exports exactly these public symbols:

```text
libtusk_analyze@@LIBTUSK_1
libtusk_free@@LIBTUSK_1
```

- `LIBTUSK_1` is the current shared-library symbol version.
- No other symbol in the shared object is part of the supported ABI contract.

## Building

```bash
cd /path/to/tusk
cmake -S . -B build
cmake --build build
```

Build outputs:

- `tusk_static` -> `build/libtusk.a`
- `tusk_shared` -> `build/libtusk.so`, `build/libtusk.so.1`
- `tsktool` -> standalone CLI executable

## JSON Result

`libtusk_analyze` returns a JSON string with this top-level shape:

```json
{
  "image": "/path/to/image.dd",
  "filesystem": {
    "type": "exFAT",
    "block_size": 512,
    "offset": 0
  },
  "files": [
    {
      "filename": "/path/to/file.txt",
      "type": "file",
      "is_fragmented": false,
      "size": 1024,
      "fragments": [
        {"start_offset": 2048, "end_offset": 3071}
      ]
    }
  ]
}
```

Important fields:

- `filesystem.type`: detected filesystem name
- `filesystem.block_size`: block size in bytes
- `filesystem.offset`: filesystem byte offset in the disk image
- `files[*].type`: `file`, `directory`, or `other`
- `files[*].fragments`: absolute byte ranges inside the image

## Using LibTusk From Go

### Choose the Right Form

- Use `libtusk.a` with `cgo` for static link-time integration.
- Use `libtusk.so` with `cgo` for dynamic linking.
- Use `libtusk.so` with `purego` for runtime loading without `cgo`.
- Do not try to use `libtusk.a` with `purego`; `purego` loads shared objects only.

### Go With `cgo` Using the Static Archive

This form forces static use of `libtusk.a` by referencing the archive directly.

```go
package libtusk

/*
#cgo CFLAGS: -I/path/to/tusk/include
#cgo LDFLAGS: /path/to/tusk/build/libtusk.a /path/to/tusk/lib/libtsk.a /path/to/tusk/lib/libz.a -lstdc++ -lpthread

#include "libtusk.h"
#include <stdlib.h>
*/
import "C"

import (
    "fmt"
    "unsafe"
)

func AnalyzeImage(imagePath string) (string, error) {
    cPath := C.CString(imagePath)
    defer C.free(unsafe.Pointer(cPath))

    jsonPtr := C.libtusk_analyze(cPath)
    if jsonPtr == nil {
        return "", fmt.Errorf("libtusk_analyze failed for %q", imagePath)
    }
    defer C.libtusk_free(jsonPtr)

    return C.GoString(jsonPtr), nil
}
```

Notes:

- The static build currently depends on `/path/to/tusk/lib/libtsk.a` and `/path/to/tusk/lib/libz.a`.
- `-lstdc++` is required because `libtusk` is implemented in C++.

Build example:

```bash
CGO_ENABLED=1 go build ./...
```

If your toolchain supports fully static linking:

```bash
CGO_ENABLED=1 go build -ldflags='-extldflags=-static' ./...
```

### Go With `cgo` Using the Shared Library

This form links against `libtusk.so`.

```go
package libtusk

/*
#cgo CFLAGS: -I/path/to/tusk/include
#cgo LDFLAGS: -L/path/to/tusk/build -ltusk -lstdc++ -lpthread

#include "libtusk.h"
#include <stdlib.h>
*/
import "C"

import (
    "fmt"
    "unsafe"
)

func AnalyzeImage(imagePath string) (string, error) {
    cPath := C.CString(imagePath)
    defer C.free(unsafe.Pointer(cPath))

    jsonPtr := C.libtusk_analyze(cPath)
    if jsonPtr == nil {
        return "", fmt.Errorf("libtusk_analyze failed for %q", imagePath)
    }
    defer C.libtusk_free(jsonPtr)

    return C.GoString(jsonPtr), nil
}
```

Runtime requirements:

- `libtusk.so` must be visible to the dynamic loader.
- The host system must also provide compatible shared `libtsk.so` and `libz.so`.

Typical runtime setup:

```bash
export LD_LIBRARY_PATH=/path/to/tusk/build:${LD_LIBRARY_PATH}
go run . /path/to/image.dd
```

### Go With `purego` Using the Shared Library

`purego` can load `libtusk.so` and call the public C ABI directly.

```go
package libtusk

import (
    "fmt"
    "runtime"
    "unsafe"

    "github.com/ebitengine/purego"
)

var (
    analyze func(*byte) *byte
    freeFn  func(*byte)
)

func Open(path string) error {
    handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
    if err != nil {
        return err
    }

    purego.RegisterLibFunc(&analyze, handle, "libtusk_analyze")
    purego.RegisterLibFunc(&freeFn, handle, "libtusk_free")
    return nil
}

func AnalyzeImage(imagePath string) (string, error) {
    pathBytes := append([]byte(imagePath), 0)
    jsonPtr := analyze(&pathBytes[0])
    runtime.KeepAlive(pathBytes)

    if jsonPtr == nil {
        return "", fmt.Errorf("libtusk_analyze failed for %q", imagePath)
    }
    defer freeFn(jsonPtr)

    return copyCString(jsonPtr), nil
}

func copyCString(ptr *byte) string {
    var buf []byte
    for addr := uintptr(unsafe.Pointer(ptr)); ; addr++ {
        b := *(*byte)(unsafe.Pointer(addr))
        if b == 0 {
            return string(buf)
        }
        buf = append(buf, b)
    }
}
```

Rules:

- Pass a NUL-terminated path buffer.
- Copy the returned bytes into Go memory before the function returns.
- Always call `libtusk_free` after copying the string.

## FFI Correctness Rules

- Treat `include/libtusk.h` as the source of truth.
- Call `libtusk_free` exactly once for each non-`NULL` return value.
- Do not mutate the returned buffer.
- Do not hold the raw returned pointer after freeing it.
- Copy the string into language-managed memory if you need to retain it.

## Troubleshooting

### `undefined reference to libtusk_analyze`

- Confirm the library was built.
- Use the correct archive or shared-library search path.
- If you intended static linking, reference `build/libtusk.a` directly.

### missing C++ runtime symbols at link time

- Add `-lstdc++` to Go `#cgo LDFLAGS`.

### shared library not found at runtime

- Add `build/` to `LD_LIBRARY_PATH` or install the shared library into a loader search path.
- Ensure `libtsk.so` and `libz.so` are also available.

### `purego` returns corrupted data or leaks memory

- Copy the C string into Go memory before freeing it.
- Call `libtusk_free` for every successful call.
