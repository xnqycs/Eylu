package protocol

import "fmt"

type ErrorCode string

const (
	ErrConfig     ErrorCode = "config_error"
	ErrCredential ErrorCode = "credential_error"
	ErrNetwork    ErrorCode = "network_error"
	ErrAuth       ErrorCode = "authentication_error"
	ErrRateLimit  ErrorCode = "rate_limit_error"
	ErrProvider   ErrorCode = "provider_error"
	ErrTimeout    ErrorCode = "timeout_error"
	ErrCancelled  ErrorCode = "cancelled"
	ErrProtocol   ErrorCode = "protocol_error"
	ErrTool       ErrorCode = "tool_error"
)

type Error struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	Retryable  bool      `json:"retryable,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	Cause      error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }
