package instrumented

import (
	"context"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
)

// Store decorates a TaskStore with Postgres dependency metrics (MT-13).
// Event-derived metrics don't come from here: the store's own event
// observer feeds Metrics.Observe after each commit.
type Store struct {
	Next taskrunner.TaskStore
	M    *Metrics
}

var _ taskrunner.TaskStore = Store{}

func (s Store) done(ctx context.Context, method string, start time.Time, err error) {
	s.M.record(ctx, failure.DepPostgres, method, start, err, "")
}

func (s Store) CreateTask(ctx context.Context, task domain.Task) error {
	start := time.Now()
	err := s.Next.CreateTask(ctx, task)
	s.done(ctx, "create_task", start, err)
	return err
}

func (s Store) GetTask(ctx context.Context, taskID string) (domain.TaskRecord, bool, error) {
	start := time.Now()
	rec, ok, err := s.Next.GetTask(ctx, taskID)
	s.done(ctx, "get_task", start, err)
	return rec, ok, err
}

func (s Store) UpdateTaskState(ctx context.Context, state domain.TaskState, events ...domain.TaskEvent) error {
	start := time.Now()
	err := s.Next.UpdateTaskState(ctx, state, events...)
	s.done(ctx, "update_task_state", start, err)
	return err
}

func (s Store) SetTaskPRNumber(ctx context.Context, taskID string, prNumber int) error {
	start := time.Now()
	err := s.Next.SetTaskPRNumber(ctx, taskID, prNumber)
	s.done(ctx, "set_task_pr_number", start, err)
	return err
}

func (s Store) FailTask(ctx context.Context, rec domain.FailureRecord, events ...domain.TaskEvent) error {
	start := time.Now()
	err := s.Next.FailTask(ctx, rec, events...)
	s.done(ctx, "fail_task", start, err)
	return err
}

func (s Store) RetryTask(ctx context.Context, taskID string, maxRetries int, ev domain.TaskEvent) (domain.TaskState, error) {
	start := time.Now()
	st, err := s.Next.RetryTask(ctx, taskID, maxRetries, ev)
	if err == domain.ErrNotRetryable {
		s.done(ctx, "retry_task", start, nil) // a refused compare-and-set is not a database error
	} else {
		s.done(ctx, "retry_task", start, err)
	}
	return st, err
}

func (s Store) EnsureRound(ctx context.Context, taskID string, round int) (bool, error) {
	start := time.Now()
	created, err := s.Next.EnsureRound(ctx, taskID, round)
	s.done(ctx, "ensure_round", start, err)
	return created, err
}

func (s Store) AppendEvents(ctx context.Context, events ...domain.TaskEvent) error {
	start := time.Now()
	err := s.Next.AppendEvents(ctx, events...)
	s.done(ctx, "append_events", start, err)
	return err
}

func (s Store) ListEvents(ctx context.Context, taskID string, f domain.EventFilter) ([]domain.TaskEvent, error) {
	start := time.Now()
	evs, err := s.Next.ListEvents(ctx, taskID, f)
	s.done(ctx, "list_events", start, err)
	return evs, err
}

func (s Store) ListTasksByStatus(ctx context.Context, status string) ([]domain.TaskAge, error) {
	start := time.Now()
	out, err := s.Next.ListTasksByStatus(ctx, status)
	s.done(ctx, "list_tasks_by_status", start, err)
	return out, err
}

func (s Store) FleetSnapshot(ctx context.Context, stuckAfter time.Duration) (domain.FleetSnapshot, error) {
	start := time.Now()
	snap, err := s.Next.FleetSnapshot(ctx, stuckAfter)
	s.done(ctx, "fleet_snapshot", start, err)
	return snap, err
}

func (s Store) RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error {
	start := time.Now()
	err := s.Next.RecordStepLaunched(ctx, taskID, round, step, invocationID)
	s.done(ctx, "record_step_launched", start, err)
	return err
}

func (s Store) CompleteStepRun(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error {
	start := time.Now()
	err := s.Next.CompleteStepRun(ctx, taskID, invocationID, run, execErr)
	s.done(ctx, "complete_step_run", start, err)
	return err
}

func (s Store) ListStepRuns(ctx context.Context, taskID string, round int) ([]domain.StepRun, error) {
	start := time.Now()
	runs, err := s.Next.ListStepRuns(ctx, taskID, round)
	s.done(ctx, "list_step_runs", start, err)
	return runs, err
}

func (s Store) ListStepRunsForTask(ctx context.Context, taskID string) ([]domain.StepRun, error) {
	start := time.Now()
	runs, err := s.Next.ListStepRunsForTask(ctx, taskID)
	s.done(ctx, "list_step_runs_for_task", start, err)
	return runs, err
}

func (s Store) ListRunningTaskIDs(ctx context.Context) ([]string, error) {
	start := time.Now()
	ids, err := s.Next.ListRunningTaskIDs(ctx)
	s.done(ctx, "list_running_task_ids", start, err)
	return ids, err
}

func (s Store) FindWaitingApprovalTask(ctx context.Context, targetRepo string, issueNumber int) (string, bool, error) {
	start := time.Now()
	id, ok, err := s.Next.FindWaitingApprovalTask(ctx, targetRepo, issueNumber)
	s.done(ctx, "find_waiting_approval_task", start, err)
	return id, ok, err
}
