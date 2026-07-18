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
	fmt.Fprintf(w.writer, "[audit] request_id=%s call_id=%s tool=%s risk=%s decision=%s confirmed=%t duration_ms=%d exit_code=%d error=%t truncated=%t\n",
		record.RequestID, record.CallID, record.Tool, record.Risk, record.Decision, record.Confirmed, record.DurationMS, record.ExitCode, record.IsError, record.Truncated)
}
