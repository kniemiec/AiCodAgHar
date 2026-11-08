package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresWriter is AuditWriter's production implementation, writing to the
// audit schema via the orchestrator_audit_writer Postgres role.
type PostgresWriter struct {
	db *sql.DB
}

// NewPostgresWriter opens a connection pool against dsn (a
// postgres://orchestrator_audit_writer:...@host:port/orchestrator DSN).
func NewPostgresWriter(dsn string) (*PostgresWriter, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("audit: open db: %w", err)
	}
	return &PostgresWriter{db: db}, nil
}

// Close releases the underlying connection pool.
func (w *PostgresWriter) Close() error {
	return w.db.Close()
}

// Ping checks the pool can reach Postgres (readiness, HL-1).
func (w *PostgresWriter) Ping(ctx context.Context) error {
	return w.db.PingContext(ctx)
}

// WriteInvocationRecord extends task_id's hash chain by one row, inside a
// transaction serialized per task_id via a Postgres advisory lock — fan-out
// can write multiple invocations for the same task concurrently, and the
// chain must still extend one link at a time.
func (w *PostgresWriter) WriteInvocationRecord(ctx context.Context, rec InvocationAuditRecord) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("audit: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, rec.TaskID); err != nil {
		return fmt.Errorf("audit: acquire per-task lock: %w", err)
	}

	prevHash := GenesisHash
	err = tx.QueryRowContext(ctx,
		`SELECT row_hash FROM audit.invocation_audit WHERE task_id = $1 ORDER BY id DESC LIMIT 1`,
		rec.TaskID,
	).Scan(&prevHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("audit: read chain tail for task %s: %w", rec.TaskID, err)
	}

	rec.PrevHash = prevHash
	rowHash, err := ComputeRowHash(rec, prevHash)
	if err != nil {
		return err
	}
	rec.RowHash = rowHash

	grantedScopesJSON, err := json.Marshal(rec.GrantedScopes)
	if err != nil {
		return fmt.Errorf("audit: marshal granted_scopes for %s: %w", rec.InvocationID, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit.invocation_audit (
			invocation_id, task_id, round, step_id, step_type, bot_account, target_repo,
			policy_decision, granted_scopes, policy_bundle_version, resolved_steps,
			exit_code, status, approved, approved_by, evidence, prev_hash, row_hash
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		rec.InvocationID, rec.TaskID, rec.Round, rec.StepID, rec.StepType, rec.BotAccount, rec.TargetRepo,
		rec.PolicyDecision, grantedScopesJSON, rec.PolicyBundleVersion, nullableJSON(rec.ResolvedStepsJSON),
		rec.ExitCode, rec.Status, rec.Approved, rec.ApprovedBy, rec.Evidence, rec.PrevHash, rec.RowHash,
	)
	if err != nil {
		return fmt.Errorf("audit: insert invocation_audit row for %s: %w", rec.InvocationID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("audit: commit: %w", err)
	}
	return nil
}

func (w *PostgresWriter) GetTaskChain(ctx context.Context, taskID string) ([]InvocationAuditRecord, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT invocation_id, task_id, round, step_id, step_type, bot_account, target_repo,
		       policy_decision, granted_scopes, policy_bundle_version, resolved_steps,
		       exit_code, status, approved, approved_by, evidence, prev_hash, row_hash
		FROM audit.invocation_audit WHERE task_id = $1 ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("audit: query task chain for %s: %w", taskID, err)
	}
	defer rows.Close()

	var records []InvocationAuditRecord
	for rows.Next() {
		var rec InvocationAuditRecord
		var scopesJSON []byte
		var resolvedSteps []byte
		if err := rows.Scan(
			&rec.InvocationID, &rec.TaskID, &rec.Round, &rec.StepID, &rec.StepType, &rec.BotAccount, &rec.TargetRepo,
			&rec.PolicyDecision, &scopesJSON, &rec.PolicyBundleVersion, &resolvedSteps,
			&rec.ExitCode, &rec.Status, &rec.Approved, &rec.ApprovedBy, &rec.Evidence, &rec.PrevHash, &rec.RowHash,
		); err != nil {
			return nil, fmt.Errorf("audit: scan row for %s: %w", taskID, err)
		}
		if len(scopesJSON) > 0 {
			if err := json.Unmarshal(scopesJSON, &rec.GrantedScopes); err != nil {
				return nil, fmt.Errorf("audit: unmarshal granted_scopes for %s: %w", rec.InvocationID, err)
			}
		}
		rec.ResolvedStepsJSON = resolvedSteps
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: iterate rows for %s: %w", taskID, err)
	}
	return records, nil
}

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
