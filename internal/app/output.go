package app

import (
	"Eylu/internal/protocol"
)

// JSONEnvelopeSchemaVersion identifies the shape of the program-facing output:
// the object `--output json` writes, and every line of the `--output jsonl`
// stream.
//
// A script parses this output without being able to ask which build produced it,
// so the envelope names its own shape. Within a major version a field may be
// added - every added field is optional - but never removed and never given a
// different meaning. What this output promises and what it does not is stated in
// `docs/compatibility.md`, and `go test ./internal/app/` enforces the field
// names against the committed contract.
const JSONEnvelopeSchemaVersion = 1

// jsonlLine builds one line of the `--output jsonl` stream.
//
// Every line goes through here so that the type and the schema version are
// produced together: a line that forgot the version would be indistinguishable
// from a line of an older shape, which is the failure this exists to prevent.
func jsonlLine(kind string, fields map[string]any) map[string]any {
	line := make(map[string]any, len(fields)+2)
	line["type"] = kind
	line["schema_version"] = JSONEnvelopeSchemaVersion
	for key, value := range fields {
		line[key] = value
	}
	return line
}

// jsonResponse is the object `--output json` writes for a finished request.
//
// The model response keeps every field it had, under the same names, and the
// request totals and the schema version are added beside them: a reader that only
// knows the older shape is unaffected, and a reader that wants the cost of the
// whole request no longer has to mistake the last call for it.
type jsonResponse struct {
	protocol.ModelResponse
	RequestUsage      protocol.Usage `json:"request_usage"`
	RequestModelCalls int            `json:"request_model_calls"`
	SchemaVersion     int            `json:"schema_version"`
}

// jsonError is the object `--output json` writes for a failed request. It has no
// `type` field: the json form reports a single result, where the jsonl form has
// to say which kind of line each one is.
type jsonError struct {
	Error         map[string]any `json:"error"`
	SchemaVersion int            `json:"schema_version"`
}
