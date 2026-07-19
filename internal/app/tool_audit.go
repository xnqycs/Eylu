package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"Eylu/internal/tool"
)

type toolAuditWriter struct {
	mu     sync.Mutex
	writer io.Writer
	jsonl  bool
}

func (w *toolAuditWriter) Record(record tool.AuditRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.jsonl {
		_ = json.NewEncoder(w.writer).Encode(map[string]any{"type": "tool_audit", "audit": record})
		return
	}
	fmt.Fprintf(w.writer, "[audit] request_id=%s call_id=%s tool=%s mode=%s risk=%s class=%s decision=%s confirmations=%d warning=%t duration_ms=%d exit_code=%d error=%t truncated=%t\n",
		record.RequestID, record.CallID, record.Tool, record.Mode, record.Risk, record.Classification, record.Decision, record.Confirmations, record.Warning, record.DurationMS, record.ExitCode, record.IsError, record.Truncated)
	if record.SkillActivated != "" {
		fmt.Fprintf(w.writer, "[skill] name=%s source=%s digest=%s trigger=%s activated_at=%s allowed_tools=%q\n", record.SkillName, record.SkillSource, record.SkillDigest, record.SkillTrigger, record.SkillActivated, record.AllowedTools)
	} else if record.SkillResource != "" {
		fmt.Fprintf(w.writer, "[skill-resource] name=%s digest=%s path=%s bytes=%d\n", record.SkillName, record.SkillDigest, record.SkillResource, record.ResourceBytes)
	}
}
