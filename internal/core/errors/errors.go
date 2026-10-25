package errors

import (
	"context"
	stderrors "errors"

	"indicer/internal/safe"
)

type Code string

const (
	CodeInvalidRequest   Code = "invalid_request"
	CodeUnknownProduct   Code = "unknown_product"
	CodeUnknownOperation Code = "unknown_operation"
	CodeInternal         Code = "internal_error"
	CodeNotImplemented   Code = "not_implemented"
	CodeCanceled         Code = "canceled"
	CodeLimitExceeded    Code = "limit_exceeded"
	// CodeInvalidArchive: the archive cannot be extracted here as it stands
	// (unsafe or invalid entry names, duplicate entries, damaged data).
	CodeInvalidArchive Code = "invalid_archive"
	// CodeConflict: the output already holds what an entry would replace,
	// and the request does not allow replacing it.
	CodeConflict Code = "conflict"
)

type Error struct {
	Code    Code              `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func NewWithDetails(code Code, message string, details map[string]string) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// FromPanic is the internal error for a recovered panic. Only its ID reaches
// the caller; the log records it with the value and stack.
func FromPanic(err error) *Error {
	var pe *safe.PanicError
	if stderrors.As(err, &pe) {
		return NewWithDetails(CodeInternal, "internal error (panic "+pe.ID+")", map[string]string{"panic_id": pe.ID})
	}
	return New(CodeInternal, "internal error")
}

// FromContext describes why ctx ended: reason "timeout" when its deadline
// passed, "canceled" otherwise.
func FromContext(ctx context.Context) *Error {
	reason := "canceled"
	if stderrors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = "timeout"
	}
	return NewWithDetails(CodeCanceled, "operation canceled", map[string]string{"reason": reason})
}
