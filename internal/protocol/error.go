package protocol

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type ErrorCode string

const (
	ErrConfig          ErrorCode = "config_error"
	ErrCredential      ErrorCode = "credential_error"
	ErrNetwork         ErrorCode = "network_error"
	ErrAuth            ErrorCode = "authentication_error"
	ErrRateLimit       ErrorCode = "rate_limit_error"
	ErrProvider        ErrorCode = "provider_error"
	ErrTimeout         ErrorCode = "timeout_error"
	ErrCancelled       ErrorCode = "cancelled"
	ErrProtocol        ErrorCode = "protocol_error"
	ErrTool            ErrorCode = "tool_error"
	ErrContextWindow   ErrorCode = "context_window_error"
	ErrUnsupportedTool ErrorCode = "unsupported_tool"
)

type Error struct {
	Code         ErrorCode `json:"code"`
	Message      string    `json:"message"`
	Retryable    bool      `json:"retryable,omitempty"`
	StatusCode   int       `json:"status_code,omitempty"`
	ContextLimit int       `json:"context_limit,omitempty"`
	Cause        error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length(?: is| of|:)?\s*([0-9][0-9,]*)`),
	regexp.MustCompile(`(?i)(?:max(?:imum)?[_ ]context(?:[_ ]length)?|context[_ ]window)(?: is| of|:|=)?\s*([0-9][0-9,]*)`),
}

func ClassifyProviderHTTPError(status int, message string) *Error {
	lower := strings.ToLower(message)
	contextFailure := status == 400 || status == 413 || status == 422
	contextFailure = contextFailure && (strings.Contains(lower, "context_length_exceeded") || strings.Contains(lower, "context length") || strings.Contains(lower, "context window") || strings.Contains(lower, "too many tokens") || strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "input is too long") || strings.Contains(lower, "input too long"))
	if contextFailure {
		limit := 0
		for _, pattern := range contextLimitPatterns {
			match := pattern.FindStringSubmatch(message)
			if len(match) == 2 {
				limit, _ = strconv.Atoi(strings.ReplaceAll(match[1], ",", ""))
				break
			}
		}
		return &Error{Code: ErrContextWindow, Message: fmt.Sprintf("provider HTTP %d: %s", status, message), StatusCode: status, ContextLimit: limit}
	}
	code, retryable := ErrProvider, status >= 500
	if status == 401 || status == 403 {
		code = ErrAuth
	}
	if status == 429 {
		code, retryable = ErrRateLimit, true
	}
	return &Error{Code: code, Message: fmt.Sprintf("provider HTTP %d: %s", status, message), StatusCode: status, Retryable: retryable}
}

func ClassifyProviderMessage(message string) *Error {
	classified := ClassifyProviderHTTPError(400, message)
	if classified.Code == ErrContextWindow {
		classified.Message = message
		classified.StatusCode = 0
		return classified
	}
	return &Error{Code: ErrProvider, Message: message}
}
