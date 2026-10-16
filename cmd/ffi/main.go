package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"unsafe"

	"indicer/internal/console"
	platformapi "indicer/pkg/api"
)

// The library runs inside a host process whose stdout and stderr are not
// DUES's to write to (FFI-3).
func init() { console.Quiet() }

const (
	emptyRequestJSON = "{}"
)

//export DuesDispatchJSON
func DuesDispatchJSON(requestJSON *C.char) *C.char {
	response := dispatchJSONPtr(requestJSON)
	return C.CString(response)
}

//export DuesFreeString
func DuesFreeString(ptr *C.char) {
	if ptr == nil {
		return
	}
	C.free(unsafe.Pointer(ptr))
}

func dispatchJSONPtr(requestJSON *C.char) string {
	request := emptyRequestJSON
	if requestJSON != nil {
		request = C.GoString(requestJSON)
	}
	return platformapi.DefaultDispatcher().DispatchJSON(context.Background(), request)
}

func main() {}
