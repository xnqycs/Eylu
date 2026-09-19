package app

import (
	"path/filepath"
	"sort"
	"testing"

	"Eylu/internal/docscheck"
	"Eylu/internal/protocol"
)

// The program-facing output is a promise to whatever parses it, and these are the
// tests that keep the promise machine-checkable: a field renamed or removed from
// the envelope fails here rather than in somebody's script.
func TestProgramFacingOutputKeepsItsCommittedFieldContract(t *testing.T) {
	t.Run("json response", func(t *testing.T) {
		docscheck.CheckFieldContract(t, filepath.Join("testdata", "json_response_format.txt"), jsonResponse{})
	})
	t.Run("json error", func(t *testing.T) {
		docscheck.CheckFieldContract(t, filepath.Join("testdata", "json_error_format.txt"), jsonError{})
	})
	t.Run("jsonl envelope", func(t *testing.T) {
		docscheck.CheckMapContract(t, filepath.Join("testdata", "jsonl_line_format.txt"), mapKeys(jsonlLine("response", map[string]any{"response": protocol.ModelResponse{}})))
	})
}

// Every jsonl line is built by the same helper, so one line's envelope is every
// line's envelope; the kinds it is used with are asserted separately, because a
// kind that stopped being emitted would remove a whole line from the stream.
func TestEveryJSONLLineCarriesTheSameEnvelope(t *testing.T) {
	kinds := []string{"routing", "context", "model_event", "request_usage", "response", "metrics", "error"}
	for _, kind := range kinds {
		line := jsonlLine(kind, map[string]any{"payload": 1})
		if line["type"] != kind {
			t.Fatalf("line type = %#v, want %q", line["type"], kind)
		}
		if line["schema_version"] != JSONEnvelopeSchemaVersion {
			t.Fatalf("%s line schema_version = %#v", kind, line["schema_version"])
		}
		if _, present := line["payload"]; !present {
			t.Fatalf("%s line lost its payload: %#v", kind, line)
		}
	}
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
