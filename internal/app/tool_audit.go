package app

import (
	"fmt"
	"io"
	"sync"

	"Eylu/internal/tool"
)

type toolAuditWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *toolAuditWriter) Record(record tool.AuditRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.writer, "[audit] request_id=%s call_id=%s tool=%s mode=%s risk=%s class=%s decision=%s confirmations=%d warning=%t duration_ms=%d exit_code=%d error=%t truncated=%t\n",
		record.RequestID, record.CallID, record.Tool, record.Mode, record.Risk, record.Classification, record.Decision, record.Confirmations, record.Warning, record.DurationMS, record.ExitCode, record.IsError, record.Truncated)
}
