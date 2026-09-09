package fakes

import (
	"context"
	"sync"

	"github.com/kniemiec/AiCodAgHar/internal/audit"
)

// FakeAuditWriter is an in-memory AuditWriter. It stores records in append
// order and never lets a write fail, mirroring the real INSERT-only posture
// without needing Postgres.
type FakeAuditWriter struct {
	mu      sync.Mutex
	records []audit.InvocationAuditRecord
}

func (f *FakeAuditWriter) WriteInvocationRecord(ctx context.Context, rec audit.InvocationAuditRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return nil
}

func (f *FakeAuditWriter) GetTaskChain(ctx context.Context, taskID string) ([]audit.InvocationAuditRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []audit.InvocationAuditRecord
	for _, r := range f.records {
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	return out, nil
}
