package tool

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"Eylu/internal/docscheck"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

// TestAuditRecordKeepsItsCommittedFieldContract is the machine-checkable half of
// the audit trail's promise.
//
// An audit record is read for compliance long after the build that wrote it, by
// a program that cannot ask which version it is parsing. The record carries a
// schema_version for that reason, and this test is what keeps the version
// honest: a field added to the struct has to be recorded in the contract, and a
// field removed or renamed fails here instead of in somebody's parser.
func TestAuditRecordKeepsItsCommittedFieldContract(t *testing.T) {
	docscheck.CheckFieldContract(t, filepath.Join("testdata", "audit_record_format.txt"), populatedAuditRecord())
}

// TestAuditRecordsNameTheirSchemaVersion covers the construction sites rather
// than the struct: the version is only useful if every record the host writes
// actually carries it.
func TestAuditRecordsNameTheirSchemaVersion(t *testing.T) {
	writer, err := NewWriteFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &capturingAuditSink{}
	executor := &Executor{Registry: NewRegistry(writer), Policy: policy.AllowAllChecker{}, Audit: sink}
	executor.ExecuteBatchOutcome(t.Context(), "request", []protocol.ToolCall{
		{ID: "call", Name: "write_file", Arguments: json.RawMessage(`{"path":"one.txt","content":"one","reason":"test"}`)},
	}, BatchHooks{}, 1)
	if len(sink.records) == 0 {
		t.Fatal("the executor recorded no audit record")
	}
	for index, record := range sink.records {
		if record.SchemaVersion != AuditSchemaVersion {
			t.Fatalf("record %d has schema_version %d, want %d", index, record.SchemaVersion, AuditSchemaVersion)
		}
	}
	// The version also reaches a reader: it is a serialized field, not a field
	// the encoder skips.
	encoded, err := json.Marshal(sink.records[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["schema_version"] != float64(AuditSchemaVersion) {
		t.Fatalf("the serialized record carries schema_version=%#v", decoded["schema_version"])
	}
}

type capturingAuditSink struct{ records []AuditRecord }

func (s *capturingAuditSink) Record(record AuditRecord) { s.records = append(s.records, record) }

// populatedAuditRecord sets every field of the struct, so the contract test
// covers the whole record rather than the subset one code path happened to fill.
func populatedAuditRecord() AuditRecord {
	return AuditRecord{
		SchemaVersion: AuditSchemaVersion,
		Timestamp:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RequestID:     "request", SessionID: "session", ProviderName: "provider", ProviderGeneration: 2, Model: "model",
		CallID: "call", ParentCallID: "parent", BatchID: "batch", BatchIndex: 1,
		Tool: "write_file", Risk: policy.RiskWrite, Decision: policy.DecisionAllow, Reason: "reason",
		Confirmed: true, DurationMS: 3, QueueDurationMS: 1, ExecutionDurationMS: 2, ConcurrencyMode: string(ConcurrencyExclusive),
		ResourceClaims: []ResourceClaim{{Kind: ResourceFile, Path: "one.txt", Access: ResourceWrite}}, IsError: false, Truncated: false,
		InputBytes: 1, OutputBytes: 2, ExitCode: 0, Mode: "manual", Classification: policy.CommandNotApplicable,
		PolicyRule: "rule", PolicySource: "source", PolicyOverride: "override", Confirmations: 1, Warning: false,
		SkillName: "skill", SkillSource: "user_eylu", SkillDigest: "digest", SkillTrigger: "model",
		SkillActivated: "2026-01-02T03:04:05Z", AllowedTools: "read_file", SkillResource: "references/guide.md",
		ResourceBytes: 4, WebBackend: "hosted", WebTarget: "search", WebStatus: "ok", WebSources: 1,
		WebInputTokens: 5, WebOutputTokens: 6, WebCostUSD: 0.01, UntrustedWebContent: true,
	}
}
