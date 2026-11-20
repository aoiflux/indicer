package ffi

import (
	"context"

	platformapi "indicer/pkg/api"
)

func DispatchJSON(requestJSON string) string {
	return platformapi.DefaultDispatcher().DispatchJSON(context.Background(), requestJSON)
}
