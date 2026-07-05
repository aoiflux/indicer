package errors

type Code string

const (
	CodeInvalidRequest   Code = "invalid_request"
	CodeUnknownProduct   Code = "unknown_product"
	CodeUnknownOperation Code = "unknown_operation"
	CodeInternal         Code = "internal_error"
	CodeNotImplemented   Code = "not_implemented"
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
