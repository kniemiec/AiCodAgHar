package taskrunner

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
)

// PostgresTaskStore is TaskStore's production implementation, writing to
// the orchestrator schema via the orchestrator_app Postgres role. It
// mirrors internal/audit.PostgresWriter's conventions exactly: database/sql
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

func (s *PostgresTaskStore) CreateTask(ctx context.Context, task domain.Task) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator.tasks (
			id, type, target_repo, triggering_principal, bot_account, risk_tier,
			branch, base_branch, pr_number, status, current_round
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'running',0)`,
		task.ID, task.Type, task.TargetRepo, task.TriggeringPrincipal, task.BotAccount, task.RiskTier,
		task.Branch, task.BaseBranch, task.PRNumber,
	)
	if err != nil {
		return fmt.Errorf("taskrunner: insert task %s: %w", task.ID, err)
	}
	return nil
}

func (s *PostgresTaskStore) GetTask(ctx context.Context, taskID string) (domain.TaskRecord, bool, error) {
	var rec domain.TaskRecord
	var approvalIssue sql.NullInt32
	var approvedBy, reason sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, type, target_repo, triggering_principal, bot_account, risk_tier,
		       branch, base_branch, pr_number,
		       status, current_round, approval_issue_number, approved_by, prior_findings, reason, updated_at
		FROM orchestrator.tasks WHERE id = $1`, taskID,
	).Scan(
		&rec.Task.ID, &rec.Task.Type, &rec.Task.TargetRepo, &rec.Task.TriggeringPrincipal, &rec.Task.BotAccount, &rec.Task.RiskTier,
		&rec.Task.Branch, &rec.Task.BaseBranch, &rec.Task.PRNumber,
		&rec.State.Status, &rec.State.CurrentRound, &approvalIssue, &approvedBy, &rec.State.PriorFindings, &reason, &rec.State.UpdatedAt,
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
	return rec, true, nil
}

func (s *PostgresTaskStore) UpdateTaskState(ctx context.Context, state domain.TaskState) error {
	var approvalIssue any
	if state.ApprovalIssueNumber != nil {
		approvalIssue = *state.ApprovalIssueNumber
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestrator.tasks
		SET status = $2, current_round = $3, approval_issue_number = $4, approved_by = $5,
		    prior_findings = $6, reason = $7, updated_at = now()
		WHERE id = $1`,
		state.TaskID, state.Status, state.CurrentRound, approvalIssue, nullableString(state.ApprovedBy),
		nullableJSON(state.PriorFindings), nullableString(state.Reason),
	)
	if err != nil {
		return fmt.Errorf("taskrunner: update task state %s: %w", state.TaskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("taskrunner: update task state %s: no such task", state.TaskID)
	}
	return nil
}

func (s *PostgresTaskStore) EnsureRound(ctx context.Context, taskID string, round int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator.rounds (task_id, round_number) VALUES ($1, $2)
		ON CONFLICT (task_id, round_number) DO NOTHING`, taskID, round)
	if err != nil {
		return fmt.Errorf("taskrunner: ensure round %d for %s: %w", round, taskID, err)
	}
	return nil
}

func (s *PostgresTaskStore) RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator.step_runs (task_id, round_number, step_id, step_type, invocation_id, blocking, status)
		VALUES ($1,$2,$3,$4,$5,$6,'launched')
		ON CONFLICT (invocation_id) DO NOTHING`,
		taskID, round, step.StepID, step.Type, invocationID, step.Blocking,
	)
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

	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestrator.step_runs
		SET status = $2, exit_code = $3, has_finding = $4, structured_result = $5, updated_at = now()
		WHERE invocation_id = $1`,
		invocationID, status, exitCode, hasFinding, structuredResult,
	)
	if err != nil {
		return fmt.Errorf("taskrunner: complete step_run %s for %s: %w", invocationID, taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("taskrunner: complete step_run %s for %s: no such row", invocationID, taskID)
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
