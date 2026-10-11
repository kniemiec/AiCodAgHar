package taskrunner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// PostgresTaskStore is TaskStore's production implementation, writing to
// the orchestrator schema via the orchestrator_app Postgres role. It
// mirrors internal/control-plane/audit.PostgresWriter's conventions exactly: database/sql
// + the pgx/v5/stdlib driver, raw SQL, its own connection pool opened from
// a DSN rather than a shared one passed in.
type PostgresTaskStore struct {
	db *sql.DB
}

// NewPostgresTaskStore opens a connection pool against dsn (a
// postgres://orchestrator_app:...@host:port/orchestrator DSN).
func NewPostgresTaskStore(dsn string) (*PostgresTaskStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: open db: %w", err)
	}
	return &PostgresTaskStore{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *PostgresTaskStore) Close() error {
	return s.db.Close()
}

// execer is the part of *sql.DB / *sql.Tx the helpers below need.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// withTx runs fn in one transaction: commit on nil, rollback otherwise. It is
// what makes "state change and its event" atomic (design §3, TS-3).
func (s *PostgresTaskStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// insertEvents appends events to orchestrator.task_events via ex.
func insertEvents(ctx context.Context, ex execer, events []domain.TaskEvent) error {
	for _, e := range events {
		payload := e.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal %s payload: %w", e.Type, err)
		}
		if _, err := ex.ExecContext(ctx, `
			INSERT INTO orchestrator.task_events (task_id, round, step_id, invocation_id, type, payload, trace_id, span_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			e.TaskID, e.Round, nullableString(e.StepID), nullableString(e.InvocationID), e.Type, b,
			nullableString(e.TraceID), nullableString(e.SpanID),
		); err != nil {
			return fmt.Errorf("insert %s event: %w", e.Type, err)
		}
	}
	return nil
}

func (s *PostgresTaskStore) CreateTask(ctx context.Context, task domain.Task) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO orchestrator.tasks (
				id, type, target_repo, triggering_principal, bot_account, risk_tier,
				branch, base_branch, pr_number, description, status, current_round
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'running',0)`,
			task.ID, task.Type, task.TargetRepo, task.TriggeringPrincipal, task.BotAccount, task.RiskTier,
			task.Branch, task.BaseBranch, task.PRNumber, task.Description,
		); err != nil {
			return err
		}
		return insertEvents(ctx, tx, []domain.TaskEvent{domain.TaskAcceptedEvent(task)})
	})
	if err != nil {
		return fmt.Errorf("taskrunner: insert task %s: %w", task.ID, err)
	}
	return nil
}

func (s *PostgresTaskStore) GetTask(ctx context.Context, taskID string) (domain.TaskRecord, bool, error) {
	var rec domain.TaskRecord
	var approvalIssue sql.NullInt32
	var approvedBy, reason, errorClass sql.NullString
	var retryable sql.NullBool
	var finishedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, type, target_repo, triggering_principal, bot_account, risk_tier,
		       branch, base_branch, pr_number, description,
		       status, current_round, approval_issue_number, approved_by, prior_findings, reason, updated_at,
		       error_class, retryable, retry_count, created_at, finished_at
		FROM orchestrator.tasks WHERE id = $1`, taskID,
	).Scan(
		&rec.Task.ID, &rec.Task.Type, &rec.Task.TargetRepo, &rec.Task.TriggeringPrincipal, &rec.Task.BotAccount, &rec.Task.RiskTier,
		&rec.Task.Branch, &rec.Task.BaseBranch, &rec.Task.PRNumber, &rec.Task.Description,
		&rec.State.Status, &rec.State.CurrentRound, &approvalIssue, &approvedBy, &rec.State.PriorFindings, &reason, &rec.State.UpdatedAt,
		&errorClass, &retryable, &rec.State.RetryCount, &rec.State.AcceptedAt, &finishedAt,
	)
	if err == sql.ErrNoRows {
		return domain.TaskRecord{}, false, nil
	}
	if err != nil {
		return domain.TaskRecord{}, false, fmt.Errorf("taskrunner: get task %s: %w", taskID, err)
	}

	rec.State.TaskID = rec.Task.ID
	if approvalIssue.Valid {
		v := int(approvalIssue.Int32)
		rec.State.ApprovalIssueNumber = &v
	}
	rec.State.ApprovedBy = approvedBy.String
	rec.State.Reason = reason.String
	rec.State.ErrorClass = errorClass.String
	rec.State.Retryable = retryable.Bool
	if finishedAt.Valid {
		t := finishedAt.Time
		rec.State.FinishedAt = &t
	}
	return rec, true, nil
}

func (s *PostgresTaskStore) UpdateTaskState(ctx context.Context, state domain.TaskState, events ...domain.TaskEvent) error {
	var approvalIssue any
	if state.ApprovalIssueNumber != nil {
		approvalIssue = *state.ApprovalIssueNumber
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE orchestrator.tasks
			SET status = $2, current_round = $3, approval_issue_number = $4, approved_by = $5,
			    prior_findings = $6, reason = $7, updated_at = now(),
			    finished_at = CASE WHEN $2 IN ('stop_success', 'stop_escalate') THEN now() ELSE finished_at END
			WHERE id = $1`,
			state.TaskID, state.Status, state.CurrentRound, approvalIssue, nullableString(state.ApprovedBy),
			nullableJSON(state.PriorFindings), nullableString(state.Reason),
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no such task")
		}
		return insertEvents(ctx, tx, events)
	})
	if err != nil {
		return fmt.Errorf("taskrunner: update task state %s: %w", state.TaskID, err)
	}
	return nil
}

func (s *PostgresTaskStore) FailTask(ctx context.Context, rec domain.FailureRecord, events ...domain.TaskEvent) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE orchestrator.tasks
			SET status = 'failed', error_class = $2, retryable = $3, reason = $4, updated_at = now(),
			    finished_at = CASE WHEN $3 THEN NULL ELSE now() END
			WHERE id = $1`,
			rec.TaskID, rec.Class, rec.Retryable, nullableString(rec.Reason),
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no such task")
		}
		return insertEvents(ctx, tx, events)
	})
	if err != nil {
		return fmt.Errorf("taskrunner: fail task %s: %w", rec.TaskID, err)
	}
	return nil
}

func (s *PostgresTaskStore) RetryTask(ctx context.Context, taskID string, maxRetries int, ev domain.TaskEvent) (domain.TaskState, error) {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var previous sql.NullString
		err := tx.QueryRowContext(ctx, `
			WITH prev AS (SELECT error_class FROM orchestrator.tasks WHERE id = $1 FOR UPDATE)
			UPDATE orchestrator.tasks t
			SET status = 'running', error_class = NULL, retryable = NULL, reason = NULL,
			    retry_count = retry_count + 1, finished_at = NULL, updated_at = now()
			FROM prev
			WHERE t.id = $1 AND t.status = 'failed' AND t.retryable AND t.retry_count < $2
			RETURNING prev.error_class`, taskID, maxRetries).Scan(&previous)
		if err == sql.ErrNoRows {
			return domain.ErrNotRetryable
		}
		if err != nil {
			return err
		}
		if ev.Payload == nil {
			ev.Payload = map[string]any{}
		}
		ev.Payload["previous_class"] = previous.String
		return insertEvents(ctx, tx, []domain.TaskEvent{ev})
	})
	if err != nil {
		if err == domain.ErrNotRetryable {
			return domain.TaskState{}, err
		}
		return domain.TaskState{}, fmt.Errorf("taskrunner: retry task %s: %w", taskID, err)
	}
	rec, ok, err := s.GetTask(ctx, taskID)
	if err != nil || !ok {
		return domain.TaskState{}, fmt.Errorf("taskrunner: retry task %s: reload: %v", taskID, err)
	}
	return rec.State, nil
}

func (s *PostgresTaskStore) AppendEvents(ctx context.Context, events ...domain.TaskEvent) error {
	if len(events) == 0 {
		return nil
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error { return insertEvents(ctx, tx, events) })
	if err != nil {
		return fmt.Errorf("taskrunner: append events: %w", err)
	}
	return nil
}

func (s *PostgresTaskStore) ListEvents(ctx context.Context, taskID string, f domain.EventFilter) ([]domain.TaskEvent, error) {
	q := `SELECT id, at, task_id, round, step_id, invocation_id, type, payload, trace_id, span_id
	      FROM orchestrator.task_events WHERE task_id = $1 AND id > $2`
	args := []any{taskID, f.AfterID}
	if f.Type != "" {
		args = append(args, f.Type)
		q += fmt.Sprintf(" AND type = $%d", len(args))
	}
	if !f.Since.IsZero() {
		args = append(args, f.Since)
		q += fmt.Sprintf(" AND at >= $%d", len(args))
	}
	if !f.Until.IsZero() {
		args = append(args, f.Until)
		q += fmt.Sprintf(" AND at <= $%d", len(args))
	}
	q += " ORDER BY id ASC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: list events for %s: %w", taskID, err)
	}
	defer rows.Close()

	var out []domain.TaskEvent
	for rows.Next() {
		var e domain.TaskEvent
		var stepID, invID, traceID, spanID sql.NullString
		var payload []byte
		if err := rows.Scan(&e.ID, &e.At, &e.TaskID, &e.Round, &stepID, &invID, &e.Type, &payload, &traceID, &spanID); err != nil {
			return nil, fmt.Errorf("taskrunner: scan event for %s: %w", taskID, err)
		}
		e.StepID, e.InvocationID, e.TraceID, e.SpanID = stepID.String, invID.String, traceID.String, spanID.String
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &e.Payload); err != nil {
				return nil, fmt.Errorf("taskrunner: decode %s payload for %s: %w", e.Type, taskID, err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskrunner: iterate events for %s: %w", taskID, err)
	}
	return out, nil
}

func (s *PostgresTaskStore) ListTasksByStatus(ctx context.Context, status string) ([]domain.TaskAge, error) {
	sinceType := ""
	switch status {
	case domain.TaskStatusWaitingApproval:
		sinceType = domain.EventApprovalRequested
	case domain.TaskStatusFailed:
		sinceType = domain.EventTaskFailed
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, COALESCE((SELECT max(e.at) FROM orchestrator.task_events e WHERE e.task_id = t.id AND e.type = $2), t.updated_at)
		FROM orchestrator.tasks t WHERE t.status = $1 ORDER BY 2 ASC`, status, sinceType)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: list %s tasks: %w", status, err)
	}
	defer rows.Close()
	var out []domain.TaskAge
	for rows.Next() {
		a := domain.TaskAge{Status: status}
		var since time.Time
		if err := rows.Scan(&a.TaskID, &since); err != nil {
			return nil, fmt.Errorf("taskrunner: scan %s task: %w", status, err)
		}
		a.Since = since
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresTaskStore) SetTaskPRNumber(ctx context.Context, taskID string, prNumber int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE orchestrator.tasks SET pr_number = $2 WHERE id = $1`, taskID, prNumber)
	if err != nil {
		return fmt.Errorf("taskrunner: set pr_number for %s: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("taskrunner: set pr_number for %s: no such task", taskID)
	}
	return nil
}

func (s *PostgresTaskStore) EnsureRound(ctx context.Context, taskID string, round int) (bool, error) {
	var created bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO orchestrator.rounds (task_id, round_number) VALUES ($1, $2)
			ON CONFLICT (task_id, round_number) DO NOTHING`, taskID, round)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		created = n == 1
		if !created {
			return nil
		}
		return insertEvents(ctx, tx, []domain.TaskEvent{domain.RoundStartedEvent(taskID, round)})
	})
	if err != nil {
		return false, fmt.Errorf("taskrunner: ensure round %d for %s: %w", round, taskID, err)
	}
	return created, nil
}

func (s *PostgresTaskStore) RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO orchestrator.step_runs (task_id, round_number, step_id, step_type, invocation_id, blocking, status)
			VALUES ($1,$2,$3,$4,$5,$6,'launched')
			ON CONFLICT (invocation_id) DO NOTHING`,
			taskID, round, step.StepID, step.Type, invocationID, step.Blocking,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // already recorded; no second step.launched
		}
		return insertEvents(ctx, tx, []domain.TaskEvent{
			domain.StepLaunchedEvent(taskID, round, step.StepID, invocationID, step.Type, step.Phase, step.Blocking),
		})
	})
	if err != nil {
		return fmt.Errorf("taskrunner: record step %s launched for %s: %w", step.StepID, taskID, err)
	}
	return nil
}

func (s *PostgresTaskStore) CompleteStepRun(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error {
	status := domain.StepRunStatusCompleted
	var exitCode any
	var hasFinding any
	var structuredResult any
	if execErr != nil {
		status = domain.StepRunStatusFailed
	} else {
		exitCode = run.ExitCode
		hasFinding = run.HasFinding
		structuredResult = nullableJSON(run.StructuredResult)
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var round int
		var stepID string
		var durationMS int64
		err := tx.QueryRowContext(ctx, `
			UPDATE orchestrator.step_runs
			SET status = $2, exit_code = $3, has_finding = $4, structured_result = $5, updated_at = now()
			WHERE invocation_id = $1
			RETURNING round_number, step_id, (extract(epoch FROM (now() - created_at)) * 1000)::bigint`,
			invocationID, status, exitCode, hasFinding, structuredResult,
		).Scan(&round, &stepID, &durationMS)
		if err == sql.ErrNoRows {
			return fmt.Errorf("no such row")
		}
		if err != nil {
			return err
		}
		return insertEvents(ctx, tx, []domain.TaskEvent{
			domain.StepResolvedEvent(taskID, round, stepID, invocationID, run, execErr, time.Duration(durationMS)*time.Millisecond),
		})
	})
	if err != nil {
		return fmt.Errorf("taskrunner: complete step_run %s for %s: %w", invocationID, taskID, err)
	}
	return nil
}

func (s *PostgresTaskStore) ListStepRuns(ctx context.Context, taskID string, round int) ([]domain.StepRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT round_number, step_id, step_type, invocation_id, blocking,
		       exit_code, has_finding, structured_result, status
		FROM orchestrator.step_runs WHERE task_id = $1 AND round_number = $2 ORDER BY id ASC`,
		taskID, round,
	)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: list step_runs for %s round %d: %w", taskID, round, err)
	}
	defer rows.Close()

	var runs []domain.StepRun
	for rows.Next() {
		var run domain.StepRun
		var exitCode sql.NullInt32
		var hasFinding sql.NullBool
		if err := rows.Scan(
			&run.Round, &run.StepID, &run.StepType, &run.InvocationID, &run.Blocking,
			&exitCode, &hasFinding, &run.StructuredResult, &run.Status,
		); err != nil {
			return nil, fmt.Errorf("taskrunner: scan step_run for %s round %d: %w", taskID, round, err)
		}
		run.ExitCode = exitCode.Int32
		run.HasFinding = hasFinding.Bool
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskrunner: iterate step_runs for %s round %d: %w", taskID, round, err)
	}
	return runs, nil
}

func (s *PostgresTaskStore) ListStepRunsForTask(ctx context.Context, taskID string) ([]domain.StepRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT round_number, step_id, step_type, invocation_id, blocking,
		       exit_code, has_finding, structured_result, status
		FROM orchestrator.step_runs WHERE task_id = $1 ORDER BY round_number ASC, id ASC`,
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: list step_runs for %s: %w", taskID, err)
	}
	defer rows.Close()

	var runs []domain.StepRun
	for rows.Next() {
		var run domain.StepRun
		var exitCode sql.NullInt32
		var hasFinding sql.NullBool
		if err := rows.Scan(
			&run.Round, &run.StepID, &run.StepType, &run.InvocationID, &run.Blocking,
			&exitCode, &hasFinding, &run.StructuredResult, &run.Status,
		); err != nil {
			return nil, fmt.Errorf("taskrunner: scan step_run for %s: %w", taskID, err)
		}
		run.ExitCode = exitCode.Int32
		run.HasFinding = hasFinding.Bool
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskrunner: iterate step_runs for %s: %w", taskID, err)
	}
	return runs, nil
}

func (s *PostgresTaskStore) ListRunningTaskIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM orchestrator.tasks WHERE status = $1`, domain.TaskStatusRunning)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: list running tasks: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("taskrunner: scan running task id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskrunner: iterate running tasks: %w", err)
	}
	return ids, nil
}

func (s *PostgresTaskStore) FindWaitingApprovalTask(ctx context.Context, targetRepo string, issueNumber int) (string, bool, error) {
	var taskID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM orchestrator.tasks
		WHERE target_repo = $1 AND approval_issue_number = $2 AND status = $3`,
		targetRepo, issueNumber, domain.TaskStatusWaitingApproval,
	).Scan(&taskID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("taskrunner: find waiting-approval task for %s#%d: %w", targetRepo, issueNumber, err)
	}
	return taskID, true, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
