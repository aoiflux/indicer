//go:build cgo && (windows || linux) && amd64 && !notusk

package parser

/*
// libtusk_win.a on Windows is GCC/MinGW cross-compiled and requires libstdc++ and libm.
// libtusk_win.a is The Sleuth Kit static library (Windows cross-compiled).
#cgo windows LDFLAGS: ${SRCDIR}/../../clib/libtusk_win.a ${SRCDIR}/../../clib/libtusk_win.a -lstdc++ -lm
// libtusk_lnx.a on Linux is cross-compiled and requires libstdc++ and libm.
// libtusk_lnx.a is The Sleuth Kit static library (Linux cross-compiled).
#cgo linux LDFLAGS: ${SRCDIR}/../../clib/libtusk_lnx.a ${SRCDIR}/../../clib/libtusk_lnx.a -lstdc++ -lm

#include <stdlib.h>

extern char *libtusk_analyze(const char *image_path);
extern void libtusk_free(char *ptr);
*/
import "C"

import (
	"errors"
	"unsafe"
)

var errTuskFailed = errors.New("libtusk: analyze returned nil")

func tuskAvailable() bool { return true }

func tuskAnalyze(imagePath string) (string, error) {
	cpath := C.CString(imagePath)
	defer C.free(unsafe.Pointer(cpath))

	cresult := C.libtusk_analyze(cpath)
	if cresult == nil {
		return "", errTuskFailed
	}
	defer C.libtusk_free(cresult)

	return C.GoString(cresult), nil
}
