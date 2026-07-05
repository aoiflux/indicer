package jsonbridge

import (
	"encoding/json"

	coreerr "indicer/internal/core/errors"
)

const CurrentVersion = "v1"

type Request struct {
	Version   string          `json:"version"`
	Product   string          `json:"product"`
	Operation string          `json:"operation"`
	Params    json.RawMessage `json:"params"`
}

type Response struct {
	Version string         `json:"version"`
	Status  string         `json:"status"`
	Error   *coreerr.Error `json:"error,omitempty"`
	Result  any            `json:"result,omitempty"`
}

func DecodeRequest(input string) (Request, *coreerr.Error) {
	var req Request
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return Request{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "invalid JSON request", map[string]string{"cause": err.Error()})
	}
	if req.Version == "" {
		return Request{}, coreerr.New(coreerr.CodeInvalidRequest, "version is required")
	}
	if req.Version != CurrentVersion {
		return Request{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "unsupported version", map[string]string{"supported": CurrentVersion, "requested": req.Version})
	}
	if req.Product == "" {
		return Request{}, coreerr.New(coreerr.CodeInvalidRequest, "product is required")
	}
	if req.Operation == "" {
		return Request{}, coreerr.New(coreerr.CodeInvalidRequest, "operation is required")
	}
	if req.Params == nil {
		req.Params = json.RawMessage([]byte("{}"))
	}
	return req, nil
}

func EncodeOK(result any) string {
	out, _ := json.Marshal(Response{Version: CurrentVersion, Status: "ok", Result: result})
	return string(out)
}

func EncodeError(err *coreerr.Error) string {
	out, _ := json.Marshal(Response{Version: CurrentVersion, Status: "error", Error: err})
	return string(out)
}
