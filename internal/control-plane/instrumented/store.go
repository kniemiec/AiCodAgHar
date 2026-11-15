package instrumented

import (
	"context"
	"errors"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// Store decorates a TaskStore (dependency "postgres"). Event metrics do not
// come from here: the store's own after-commit Observer feeds the EventSink,
// because it alone sees the events CreateTask/EnsureRound/RecordStepLaunched/
// CompleteStepRun derive internally.
type Store struct {
	Next    taskrunner.TaskStore
	Metrics *telemetry.ControlPlane
}

var _ taskrunner.TaskStore = Store{}

func (s Store) rec(ctx context.Context, method string, start time.Time, err error) {
	record(ctx, s.Metrics, failure.DepPostgres, method, start, err)
}

func (s Store) CreateTask(ctx context.Context, task domain.Task) error {
	start := time.Now()
	err := s.Next.CreateTask(ctx, task)
	s.rec(ctx, "CreateTask", start, err)
	return err
}

func (s Store) GetTask(ctx context.Context, taskID string) (domain.TaskRecord, bool, error) {
	start := time.Now()
	rec, ok, err := s.Next.GetTask(ctx, taskID)
	s.rec(ctx, "GetTask", start, err)
	return rec, ok, err
}

func (s Store) UpdateTaskState(ctx context.Context, state domain.TaskState, events ...domain.TaskEvent) error {
	start := time.Now()
	err := s.Next.UpdateTaskState(ctx, state, events...)
	s.rec(ctx, "UpdateTaskState", start, err)
	return err
}

func (s Store) SetTaskPRNumber(ctx context.Context, taskID string, prNumber int) error {
	start := time.Now()
	err := s.Next.SetTaskPRNumber(ctx, taskID, prNumber)
	s.rec(ctx, "SetTaskPRNumber", start, err)
	return err
}

func (s Store) FailTask(ctx context.Context, rec domain.FailureRecord, events ...domain.TaskEvent) error {
	start := time.Now()
	err := s.Next.FailTask(ctx, rec, events...)
	s.rec(ctx, "FailTask", start, err)
	return err
}

func (s Store) RetryTask(ctx context.Context, taskID string, maxRetries int, ev domain.TaskEvent) (domain.TaskState, error) {
	start := time.Now()
	st, err := s.Next.RetryTask(ctx, taskID, maxRetries, ev)
	// A lost compare-and-set is an answer, not a failed call.
	if errors.Is(err, domain.ErrNotRetryable) {
		s.rec(ctx, "RetryTask", start, nil)
	} else {
		s.rec(ctx, "RetryTask", start, err)
	}
	return st, err
}

func (s Store) EnsureRound(ctx context.Context, taskID string, round int) (bool, error) {
	start := time.Now()
	created, err := s.Next.EnsureRound(ctx, taskID, round)
	s.rec(ctx, "EnsureRound", start, err)
	return created, err
}

func (s Store) AppendEvents(ctx context.Context, events ...domain.TaskEvent) error {
	start := time.Now()
	err := s.Next.AppendEvents(ctx, events...)
	s.rec(ctx, "AppendEvents", start, err)
	return err
}

func (s Store) ListEvents(ctx context.Context, taskID string, f domain.EventFilter) ([]domain.TaskEvent, error) {
	start := time.Now()
	evs, err := s.Next.ListEvents(ctx, taskID, f)
	s.rec(ctx, "ListEvents", start, err)
	return evs, err
}

func (s Store) ListTasksByStatus(ctx context.Context, status string) ([]domain.TaskAge, error) {
	start := time.Now()
	ages, err := s.Next.ListTasksByStatus(ctx, status)
	s.rec(ctx, "ListTasksByStatus", start, err)
	return ages, err
}

func (s Store) FleetSnapshot(ctx context.Context, stuckAfter time.Duration) (domain.FleetSnapshot, error) {
	start := time.Now()
	snap, err := s.Next.FleetSnapshot(ctx, stuckAfter)
	s.rec(ctx, "FleetSnapshot", start, err)
	return snap, err
}

func (s Store) RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error {
	start := time.Now()
	err := s.Next.RecordStepLaunched(ctx, taskID, round, step, invocationID)
	s.rec(ctx, "RecordStepLaunched", start, err)
	return err
}

func (s Store) CompleteStepRun(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error {
	start := time.Now()
	err := s.Next.CompleteStepRun(ctx, taskID, invocationID, run, execErr)
	s.rec(ctx, "CompleteStepRun", start, err)
	return err
}

func (s Store) ListStepRuns(ctx context.Context, taskID string, round int) ([]domain.StepRun, error) {
	start := time.Now()
	runs, err := s.Next.ListStepRuns(ctx, taskID, round)
	s.rec(ctx, "ListStepRuns", start, err)
	return runs, err
}

func (s Store) ListStepRunsForTask(ctx context.Context, taskID string) ([]domain.StepRun, error) {
	start := time.Now()
	runs, err := s.Next.ListStepRunsForTask(ctx, taskID)
	s.rec(ctx, "ListStepRunsForTask", start, err)
	return runs, err
}

func (s Store) ListRunningTaskIDs(ctx context.Context) ([]string, error) {
	start := time.Now()
	ids, err := s.Next.ListRunningTaskIDs(ctx)
	s.rec(ctx, "ListRunningTaskIDs", start, err)
	return ids, err
}

func (s Store) FindWaitingApprovalTask(ctx context.Context, targetRepo string, issueNumber int) (string, bool, error) {
	start := time.Now()
	id, ok, err := s.Next.FindWaitingApprovalTask(ctx, targetRepo, issueNumber)
	s.rec(ctx, "FindWaitingApprovalTask", start, err)
	return id, ok, err
}
