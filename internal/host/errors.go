package host

type ErrorData struct {
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}
type RPCError struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    ErrorData `json:"data"`
}

func (e *RPCError) Error() string { return e.Data.Code + ": " + e.Message }
func fault(code string) *RPCError {
	numbers := map[string]int{"PARSE_ERROR": -32700, "INVALID_REQUEST": -32600, "METHOD_NOT_FOUND": -32601, "INVALID_PARAMS": -32602, "UNSUPPORTED_VERSION": -32001, "NOT_INITIALIZED": -32002, "SESSION_BUSY": -32003, "ID_CONFLICT": -32004, "STALE_REVISION": -32005, "HOST_UNAVAILABLE": -32006, "LIMIT_EXCEEDED": -32007, "STATE_INCOMPATIBLE": -32008, "CHECKPOINT_FAILED": -32009, "UNSUPPORTED_CAPABILITY": -32010, "MODEL_REQUEST_FAILED": -32011, "NOT_FOUND": -32012}
	// Never echo untrusted parameters, provider errors or JSON decoder fragments.
	return &RPCError{Code: numbers[code], Message: code, Data: ErrorData{Code: code}}
}
