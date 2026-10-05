// Package taskrunner extracts internal/control-plane/coreloop's former per-task looping
// into a persisted, resumable state machine (docs/designs/next/full-
// pipeline-async-approval-design.md §4.2): Start kicks off round 1 in a
// goroutine and returns immediately; Resume reloads persisted state and
// continues; a task survives a control-plane restart at any point in its
// execution, not only between rounds, because TaskStore records a
// step_runs row the instant a step launches, not only once it resolves.
package taskrunner

import (
	"context"
	"fmt"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// TaskStore persists task/round/step-run state to the orchestrator schema.
// Its production implementation is PostgresTaskStore; tests use
// internal/control-plane/fakes.FakeTaskStore.
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

	// SetTaskPRNumber persists the review PR's number once coreloop opens
	// it, after the generate phase's step completes with no blocking
	// failure (design "Task Intent Propagation" §5) — a task no longer
	// arrives with pr_number already set.
	SetTaskPRNumber(ctx context.Context, taskID string, prNumber int) error

	// SetTaskReason overwrites only the task's reason column — used to
	// surface a runLoop error through GET /tasks/{id} without touching
	// status (left running, so startup reconciliation still retries it) or
	// any approval/prior-findings state UpdateTaskState would overwrite.
	SetTaskReason(ctx context.Context, taskID, reason string) error

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

	// ListStepRunsForTask returns every step_runs row for a task across all
	// of its rounds, in (round, dispatch) order — GET /tasks/{id}'s (design
	// §4.3) view of a task's full step history, as opposed to
	// ListStepRuns' single-round scope.
	ListStepRunsForTask(ctx context.Context, taskID string) ([]domain.StepRun, error)

	// ListRunningTaskIDs returns every task currently status=running — the
	// startup-reconciliation scan's entry point.
	ListRunningTaskIDs(ctx context.Context) ([]string, error)

	// FindWaitingApprovalTask looks up the task currently waiting on
	// issueNumber in targetRepo (design §4.4 step 3's webhook-handler
	// lookup). ok is false if no such task exists — already resolved, the
	// wrong repo/issue, or a stale/duplicate webhook delivery — which the
	// caller treats as a 200 no-op, not an error.
	FindWaitingApprovalTask(ctx context.Context, targetRepo string, issueNumber int) (taskID string, ok bool, err error)
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

// taskApprovalReader adapts a TaskStore into approval.TaskApprovalReader —
// the narrow interface RealApprovalVerifier needs, so internal/control-plane/approval
// doesn't import taskrunner (which imports coreloop, which imports
// approval), mirroring stepRunRecorder's adapter pattern above.
type taskApprovalReader struct {
	store TaskStore
}

// NewTaskApprovalReader adapts store into an approval.TaskApprovalReader,
// for wiring approval.RealApprovalVerifier (design §4.4) — exported because
// that wiring happens in cmd/control-plane's main.go, outside this package.
func NewTaskApprovalReader(store TaskStore) approval.TaskApprovalReader {
	return taskApprovalReader{store: store}
}

func (a taskApprovalReader) ApprovedBy(ctx context.Context, taskID string) (string, error) {
	rec, ok, err := a.store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("taskrunner: approval check: no such task %s", taskID)
	}
	return rec.State.ApprovedBy, nil
}
