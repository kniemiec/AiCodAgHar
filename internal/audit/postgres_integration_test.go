//go:build integration

package audit_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kniemiec/AiCodAgHar/internal/audit"
)

func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AUDIT_DB_DSN")
	if dsn == "" {
		t.Skip("AUDIT_DB_DSN not set; source deploy/.env first")
	}
	return dsn
}

func newTestWriter(t *testing.T) *audit.PostgresWriter {
	t.Helper()
	w, err := audit.NewPostgresWriter(requireDSN(t))
	if err != nil {
		t.Fatalf("NewPostgresWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestPostgresWriter_WriteAndReadChain(t *testing.T) {
	w := newTestWriter(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("it-task-%d", os.Getpid())

	records := []audit.InvocationAuditRecord{
		{
			InvocationID: taskID + ":round-1:pdp-eval", TaskID: taskID, Round: 1, StepType: "pdp-decision",
			PolicyDecision: "allow", GrantedScopes: []string{"write:repository"},
			// Non-empty and non-trivial on purpose: this is what caught a real
			// bug — jsonb re-serializes on storage (different whitespace than
			// Go's compact encoder), which broke hash verification on
			// read-back until resolved_steps became a text column.
			ResolvedStepsJSON: []byte(`[{"step_id":"review","type":"agent-stage","blocking":true,"bot_account":"review-bot"}]`),
		},
		{InvocationID: taskID + ":1:review", TaskID: taskID, Round: 1, StepID: "review", StepType: "agent-stage", BotAccount: "review-bot", ExitCode: 0, Status: "completed"},
		{InvocationID: taskID + ":outcome", TaskID: taskID, StepType: "task-outcome", Status: "stop-success"},
	}
	for _, rec := range records {
		if err := w.WriteInvocationRecord(ctx, rec); err != nil {
			t.Fatalf("WriteInvocationRecord(%s): %v", rec.InvocationID, err)
		}
	}

	chain, err := w.GetTaskChain(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if len(chain) != len(records) {
		t.Fatalf("expected %d rows, got %d", len(records), len(chain))
	}
	if err := audit.VerifyChain(chain); err != nil {
		t.Fatalf("expected an intact chain, got: %v", err)
	}
	if chain[0].GrantedScopes[0] != "write:repository" {
		t.Fatalf("expected granted_scopes to round-trip, got %+v", chain[0].GrantedScopes)
	}
}

// TestPostgresWriter_ConcurrentWritesSameTaskStillChain proves the
// per-task advisory lock actually serializes the hash chain: fan-out
// dispatches steps concurrently, and their audit writes for the same task
// must still extend one valid chain, not race into duplicate/broken links.
func TestPostgresWriter_ConcurrentWritesSameTaskStillChain(t *testing.T) {
	w := newTestWriter(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("it-concurrent-task-%d", os.Getpid())

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
				InvocationID: fmt.Sprintf("%s:1:step-%d", taskID, i),
				TaskID:       taskID,
				Round:        1,
				StepType:     "agent-stage",
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent write %d: %v", i, err)
		}
	}

	chain, err := w.GetTaskChain(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if len(chain) != n {
		t.Fatalf("expected %d rows, got %d", n, len(chain))
	}
	if err := audit.VerifyChain(chain); err != nil {
		t.Fatalf("expected concurrent writes to still form a valid chain, got: %v", err)
	}
}

// TestAuditWriterRole_CannotUpdate is the literal Postgres-role privilege
// test the plan calls for: attempt an UPDATE as orchestrator_audit_writer
// and confirm Postgres itself rejects it.
func TestAuditWriterRole_CannotUpdate(t *testing.T) {
	dsn := requireDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	w := newTestWriter(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("it-privtest-task-%d", os.Getpid())
	if err := w.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
		InvocationID: taskID + ":probe", TaskID: taskID, Round: 1, StepType: "agent-stage",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	_, err = db.ExecContext(ctx, `UPDATE audit.invocation_audit SET row_hash = 'tampered' WHERE task_id = $1`, taskID)
	if err == nil {
		t.Fatal("expected UPDATE as orchestrator_audit_writer to be rejected by Postgres, but it succeeded")
	}
}

// TestVerifyChain_DetectsSuperuserTampering proves the verifier catches
// tampering even when it comes from a connection with more privilege than
// orchestrator_audit_writer has (the role itself cannot UPDATE at all, per
// TestAuditWriterRole_CannotUpdate — this test shows what the verifier is
// actually for: detecting tampering that happened through some other path).
func TestVerifyChain_DetectsSuperuserTampering(t *testing.T) {
	superuserDSN := os.Getenv("AUDIT_DB_SUPERUSER_DSN")
	if superuserDSN == "" {
		t.Skip("AUDIT_DB_SUPERUSER_DSN not set; source deploy/.env first")
	}
	superuser, err := sql.Open("pgx", superuserDSN)
	if err != nil {
		t.Fatalf("open superuser conn: %v", err)
	}
	defer superuser.Close()

	w := newTestWriter(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("it-tamper-task-%d", os.Getpid())
	if err := w.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
		InvocationID: taskID + ":probe", TaskID: taskID, Round: 1, StepType: "agent-stage", Status: "completed",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	if _, err := superuser.ExecContext(ctx, `UPDATE audit.invocation_audit SET status = 'tampered' WHERE task_id = $1`, taskID); err != nil {
		t.Fatalf("superuser tamper UPDATE: %v", err)
	}

	chain, err := w.GetTaskChain(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if err := audit.VerifyChain(chain); err == nil {
		t.Fatal("expected VerifyChain to detect the superuser-tampered row, but it reported the chain as intact")
	}
}
