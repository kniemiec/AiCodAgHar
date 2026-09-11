package fakes

import (
	"context"
	"fmt"
	"sync"

	"github.com/kniemiec/AiCodAgHar/internal/audit"
)

// FakeAuditWriter is an in-memory AuditWriter. It stores records in append
// order, never lets a write fail (mirroring the real INSERT-only posture
// without needing Postgres), and maintains a real per-task_id hash chain
// via audit.ComputeRowHash — so tests exercising it can also call
// audit.VerifyChain meaningfully, the same as against PostgresWriter.
type FakeAuditWriter struct {
	mu       sync.Mutex
	records  []audit.InvocationAuditRecord
	tailHash map[string]string // task_id -> last row_hash
}

func (f *FakeAuditWriter) WriteInvocationRecord(ctx context.Context, rec audit.InvocationAuditRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tailHash == nil {
		f.tailHash = map[string]string{}
	}

	prevHash, ok := f.tailHash[rec.TaskID]
	if !ok {
		prevHash = audit.GenesisHash
	}
	rowHash, err := audit.ComputeRowHash(rec, prevHash)
	if err != nil {
		return fmt.Errorf("fakes: compute row hash for %s: %w", rec.InvocationID, err)
	}
	rec.PrevHash = prevHash
	rec.RowHash = rowHash
	f.tailHash[rec.TaskID] = rowHash

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
