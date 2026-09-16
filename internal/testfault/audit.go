package testfault

import (
	"sync"

	"Eylu/internal/tool"
)

// AuditFaults wraps a tool.AuditSink and fails it on schedule.
//
// tool.AuditSink.Record has no error return, so a panic is the only failure a
// host audit sink can raise; that is what the schedule injects. Every record is
// kept, including the one whose delivery panicked, so a test can still assert
// what the executor tried to report.
type AuditFaults struct {
	Delegate tool.AuditSink
	Fault    *Fault

	mu      sync.Mutex
	records []tool.AuditRecord
}

var _ tool.AuditSink = (*AuditFaults)(nil)

// Record keeps the record and panics when the schedule fires. A panic is not
// recovered here: whether the host callback may kill the request is exactly what
// the caller has to decide.
func (a *AuditFaults) Record(record tool.AuditRecord) {
	a.mu.Lock()
	a.records = append(a.records, record)
	a.mu.Unlock()
	if err := a.Fault.Fail(); err != nil {
		panic(err)
	}
	if a.Delegate != nil {
		a.Delegate.Record(record)
	}
}

// Records returns every record the executor produced, in order.
func (a *AuditFaults) Records() []tool.AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]tool.AuditRecord(nil), a.records...)
}
