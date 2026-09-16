// Package taskrunner extracts internal/coreloop's former per-task looping
// into a persisted, resumable state machine (docs/designs/next/full-
// pipeline-async-approval-design.md §4.2): Start kicks off round 1 in a
// goroutine and returns immediately; Resume reloads persisted state and
// continues; a task survives a control-plane restart at any point in its
// execution, not only between rounds, because TaskStore records a
// step_runs row the instant a step launches, not only once it resolves.
package taskrunner

import (
	"context"

	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
)

// TaskStore persists task/round/step-run state to the orchestrator schema.
// Its production implementation is PostgresTaskStore; tests use
// internal/fakes.FakeTaskStore.
type TaskStore interface {
	// CreateTask inserts a new task row with status=running,
	// current_round=0.
	CreateTask(ctx context.Context, task domain.Task) error

	// GetTask loads a task's immutable config joined with its live state.
	// ok is false if no such task exists.
	GetTask(ctx context.Context, taskID string) (rec domain.TaskRecord, ok bool, err error)

	// UpdateTaskState persists a task's live state in place. state.TaskID
	// selects the row; every other TaskState field is written as given.
	UpdateTaskState(ctx context.Context, state domain.TaskState) error

	// EnsureRound inserts orchestrator.rounds' (task_id, round_number) row
	// if it doesn't already exist — step_runs' foreign key requires it to
	// exist before any step_runs row for that round can be inserted.
	EnsureRound(ctx context.Context, taskID string, round int) error

	// RecordStepLaunched inserts a step_runs row the instant a step is
	// dispatched (status=launched), before its outcome is known.
	RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error

	// CompleteStepRun updates a launched step_runs row in place once the
	// step resolves. execErr is exec.Execute's own error (nil on success):
	// non-nil sets status=failed and leaves exit_code/has_finding/
	// structured_result untouched; nil sets status=completed and writes
	// run's fields.
	CompleteStepRun(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error

	// ListStepRuns returns every step_runs row for one round, in dispatch
	// order — startup reconciliation's view of what's already
	// launched/completed/failed before deciding what to (re)dispatch.
	ListStepRuns(ctx context.Context, taskID string, round int) ([]domain.StepRun, error)

	// ListRunningTaskIDs returns every task currently status=running — the
	// startup-reconciliation scan's entry point.
	ListRunningTaskIDs(ctx context.Context) ([]string, error)
}

// stepRunRecorder adapts a TaskStore into coreloop.StepRunRecorder — the
// narrow interface coreloop.Dependencies.Recorder actually needs, so
// coreloop doesn't import taskrunner (or vice versa, beyond this file).
type stepRunRecorder struct {
	store TaskStore
}

func (r stepRunRecorder) RecordLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error {
	return r.store.RecordStepLaunched(ctx, taskID, round, step, invocationID)
}

func (r stepRunRecorder) RecordCompleted(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error {
	return r.store.CompleteStepRun(ctx, taskID, invocationID, run, execErr)
}
