package jsonbridge

import (
	"encoding/json"
	"strconv"

	coreerr "indicer/internal/core/errors"
)

const CurrentVersion = "v1"

// MaxRequestBytes bounds a request envelope.
const MaxRequestBytes = 64 << 20

// FallbackError is the response when nothing else can be encoded. A caller
// never receives an empty string.
const FallbackError = `{"version":"v1","status":"error","error":{"code":"internal_error","message":"response could not be encoded"}}`

type Request struct {
	Version   string          `json:"version"`
	Product   string          `json:"product"`
	Operation string          `json:"operation"`
	Params    json.RawMessage `json:"params"`
	// TimeoutMS, when positive, bounds the operation, and for an async
	// operation the task it starts. It then ends with the error code
	// "canceled" and the reason "timeout".
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

type Response struct {
	Version string         `json:"version"`
	Status  string         `json:"status"`
	Error   *coreerr.Error `json:"error,omitempty"`
	Result  any            `json:"result,omitempty"`
}

// TooLarge is the error for a request over MaxRequestBytes.
func TooLarge() *coreerr.Error {
	return coreerr.NewWithDetails(coreerr.CodeLimitExceeded, "request too large", map[string]string{"limit_bytes": strconv.Itoa(MaxRequestBytes)})
}

func DecodeRequest(input string) (Request, *coreerr.Error) {
	if len(input) > MaxRequestBytes {
		return Request{}, TooLarge()
	}
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
	if req.TimeoutMS < 0 {
		return Request{}, coreerr.New(coreerr.CodeInvalidRequest, "timeout_ms must not be negative")
	}
	if req.Params == nil {
		req.Params = json.RawMessage([]byte("{}"))
	}
	return req, nil
}

// EncodeOK encodes a result. A result that cannot be encoded becomes an
// internal error.
func EncodeOK(result any) string {
	out, err := json.Marshal(Response{Version: CurrentVersion, Status: "ok", Result: result})
	if err != nil {
		return EncodeError(coreerr.NewWithDetails(coreerr.CodeInternal, "result could not be encoded", map[string]string{"cause": err.Error()}))
	}
	return string(out)
}

func EncodeError(err *coreerr.Error) string {
	if err == nil {
		err = coreerr.New(coreerr.CodeInternal, "unknown error")
	}
	out, merr := json.Marshal(Response{Version: CurrentVersion, Status: "error", Error: err})
	if merr != nil || len(out) == 0 {
		return FallbackError
	}
	return string(out)
}
