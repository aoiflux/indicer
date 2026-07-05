package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"unsafe"

	platformapi "indicer/pkg/api"
)

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
