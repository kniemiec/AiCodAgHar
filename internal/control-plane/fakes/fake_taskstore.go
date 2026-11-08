package fakes

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// stepRunLocation points at one row within FakeTaskStore.stepRuns, for
// CompleteStepRun to find by invocation ID without a linear scan.
type stepRunLocation struct {
	taskID string
	round  int
	idx    int
}

// FakeTaskStore is an in-memory taskrunner.TaskStore, keyed by task ID.
// Round/step_runs state is kept per (task, round) so ListStepRuns can
// return one round's rows the way orchestrator.step_runs' real foreign key
// to orchestrator.rounds would.
type FakeTaskStore struct {
	mu       sync.Mutex
	tasks    map[string]domain.TaskRecord
	rounds   map[string]map[int]bool             // task_id -> round_number -> exists
	stepRuns map[string]map[int][]domain.StepRun // task_id -> round_number -> rows, in insert order
	byInvID  map[string]stepRunLocation          // invocation_id -> location, for CompleteStepRun
	events   []domain.TaskEvent                  // task_events, in insert order (id = index+1)
	launched map[string]time.Time                // invocation_id -> launch time, for step.resolved duration_ms

	// FailEventAppend, when set, makes every write that carries events fail
	// *before* touching state — the in-memory equivalent of an event INSERT
	// violating a row inside the Postgres transaction (TS-3).
	FailEventAppend func(domain.TaskEvent) error
}

// appendEvents stamps and stores events; callers hold f.mu. It validates
// first so that a failing append leaves nothing behind.
func (f *FakeTaskStore) appendEvents(events []domain.TaskEvent) error {
	if f.FailEventAppend != nil {
		for _, e := range events {
			if err := f.FailEventAppend(e); err != nil {
				return err
			}
		}
	}
	for _, e := range events {
		e.ID = int64(len(f.events) + 1)
		e.At = time.Now()
		f.events = append(f.events, e)
	}
	return nil
}

// Events returns every recorded event for taskID in order (test helper).
func (f *FakeTaskStore) Events(taskID string) []domain.TaskEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.TaskEvent
	for _, e := range f.events {
		if e.TaskID == taskID {
			out = append(out, e)
		}
	}
	return out
}

// EventTypes returns Events(taskID)'s types, in order (test helper).
func (f *FakeTaskStore) EventTypes(taskID string) []string {
	var out []string
	for _, e := range f.Events(taskID) {
		out = append(out, e.Type)
	}
	return out
}

func (f *FakeTaskStore) init() {
	if f.tasks == nil {
		f.tasks = map[string]domain.TaskRecord{}
		f.rounds = map[string]map[int]bool{}
		f.stepRuns = map[string]map[int][]domain.StepRun{}
		f.byInvID = map[string]stepRunLocation{}
		f.launched = map[string]time.Time{}
	}
}

func (f *FakeTaskStore) CreateTask(ctx context.Context, task domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if _, ok := f.tasks[task.ID]; ok {
		return fmt.Errorf("fakes: task %s already exists", task.ID)
	}
	if err := f.appendEvents([]domain.TaskEvent{domain.TaskAcceptedEvent(task)}); err != nil {
		return err
	}
	f.tasks[task.ID] = domain.TaskRecord{
		Task:  task,
		State: domain.TaskState{TaskID: task.ID, Status: domain.TaskStatusRunning, CurrentRound: 0, AcceptedAt: time.Now()},
	}
	return nil
}

func (f *FakeTaskStore) GetTask(ctx context.Context, taskID string) (domain.TaskRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rec, ok := f.tasks[taskID]
	return rec, ok, nil
}

func (f *FakeTaskStore) UpdateTaskState(ctx context.Context, state domain.TaskState, events ...domain.TaskEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rec, ok := f.tasks[state.TaskID]
	if !ok {
		return fmt.Errorf("fakes: update task state: no such task %s", state.TaskID)
	}
	if err := f.appendEvents(events); err != nil {
		return err
	}
	// Mirror PostgresTaskStore: updated columns are the ones UpdateTaskState
	// writes; failure bookkeeping, retry_count and created_at are preserved.
	state.ErrorClass, state.Retryable, state.RetryCount, state.AcceptedAt = rec.State.ErrorClass, rec.State.Retryable, rec.State.RetryCount, rec.State.AcceptedAt
	state.FinishedAt = rec.State.FinishedAt
	if state.Status == domain.TaskStatusStopSuccess || state.Status == domain.TaskStatusStopEscalate {
		now := time.Now()
		state.FinishedAt = &now
	}
	rec.State = state
	f.tasks[state.TaskID] = rec
	return nil
}

func (f *FakeTaskStore) FailTask(ctx context.Context, fr domain.FailureRecord, events ...domain.TaskEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rec, ok := f.tasks[fr.TaskID]
	if !ok {
		return fmt.Errorf("fakes: fail task: no such task %s", fr.TaskID)
	}
	if err := f.appendEvents(events); err != nil {
		return err
	}
	rec.State.Status = domain.TaskStatusFailed
	rec.State.ErrorClass, rec.State.Retryable, rec.State.Reason = fr.Class, fr.Retryable, fr.Reason
	rec.State.FinishedAt = nil
	if !fr.Retryable {
		now := time.Now()
		rec.State.FinishedAt = &now
	}
	rec.State.UpdatedAt = time.Now()
	f.tasks[fr.TaskID] = rec
	return nil
}

func (f *FakeTaskStore) RetryTask(ctx context.Context, taskID string, maxRetries int, ev domain.TaskEvent) (domain.TaskState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rec, ok := f.tasks[taskID]
	if !ok || rec.State.Status != domain.TaskStatusFailed || !rec.State.Retryable || rec.State.RetryCount >= maxRetries {
		return domain.TaskState{}, domain.ErrNotRetryable
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	ev.Payload["previous_class"] = rec.State.ErrorClass
	if err := f.appendEvents([]domain.TaskEvent{ev}); err != nil {
		return domain.TaskState{}, err
	}
	rec.State.Status = domain.TaskStatusRunning
	rec.State.ErrorClass, rec.State.Retryable, rec.State.Reason = "", false, ""
	rec.State.RetryCount++
	rec.State.FinishedAt = nil
	rec.State.UpdatedAt = time.Now()
	f.tasks[taskID] = rec
	return rec.State, nil
}

func (f *FakeTaskStore) AppendEvents(ctx context.Context, events ...domain.TaskEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	return f.appendEvents(events)
}

func (f *FakeTaskStore) ListEvents(ctx context.Context, taskID string, flt domain.EventFilter) ([]domain.TaskEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.TaskEvent
	for _, e := range f.events {
		if e.TaskID != taskID || e.ID <= flt.AfterID {
			continue
		}
		if flt.Type != "" && e.Type != flt.Type {
			continue
		}
		if !flt.Since.IsZero() && e.At.Before(flt.Since) {
			continue
		}
		if !flt.Until.IsZero() && e.At.After(flt.Until) {
			continue
		}
		out = append(out, e)
		if flt.Limit > 0 && len(out) >= flt.Limit {
			break
		}
	}
	return out, nil
}

func (f *FakeTaskStore) ListTasksByStatus(ctx context.Context, status string) ([]domain.TaskAge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	sinceType := ""
	switch status {
	case domain.TaskStatusWaitingApproval:
		sinceType = domain.EventApprovalRequested
	case domain.TaskStatusFailed:
		sinceType = domain.EventTaskFailed
	}
	var out []domain.TaskAge
	for id, rec := range f.tasks {
		if rec.State.Status != status {
			continue
		}
		since := rec.State.UpdatedAt
		for _, e := range f.events {
			if e.TaskID == id && e.Type == sinceType && e.At.After(since) {
				since = e.At
			}
		}
		out = append(out, domain.TaskAge{TaskID: id, Status: status, Since: since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, nil
}

func (f *FakeTaskStore) SetTaskPRNumber(ctx context.Context, taskID string, prNumber int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rec, ok := f.tasks[taskID]
	if !ok {
		return fmt.Errorf("fakes: set pr_number: no such task %s", taskID)
	}
	rec.Task.PRNumber = prNumber
	f.tasks[taskID] = rec
	return nil
}

func (f *FakeTaskStore) EnsureRound(ctx context.Context, taskID string, round int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.rounds[taskID] == nil {
		f.rounds[taskID] = map[int]bool{}
	}
	if f.rounds[taskID][round] {
		return false, nil
	}
	if err := f.appendEvents([]domain.TaskEvent{domain.RoundStartedEvent(taskID, round)}); err != nil {
		return false, err
	}
	f.rounds[taskID][round] = true
	return true, nil
}

func (f *FakeTaskStore) RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if !f.rounds[taskID][round] {
		return fmt.Errorf("fakes: record step launched: round %d not started for %s", round, taskID)
	}
	if _, exists := f.byInvID[invocationID]; exists {
		return nil // ON CONFLICT DO NOTHING, matching PostgresTaskStore
	}
	if f.stepRuns[taskID] == nil {
		f.stepRuns[taskID] = map[int][]domain.StepRun{}
	}
	run := domain.StepRun{
		Round: round, StepID: step.StepID, StepType: step.Type, InvocationID: invocationID,
		Blocking: step.Blocking, Status: domain.StepRunStatusLaunched,
	}
	if err := f.appendEvents([]domain.TaskEvent{
		domain.StepLaunchedEvent(taskID, round, step.StepID, invocationID, step.Type, step.Phase, step.Blocking),
	}); err != nil {
		return err
	}
	f.launched[invocationID] = time.Now()
	f.stepRuns[taskID][round] = append(f.stepRuns[taskID][round], run)
	f.byInvID[invocationID] = stepRunLocation{taskID: taskID, round: round, idx: len(f.stepRuns[taskID][round]) - 1}
	return nil
}

func (f *FakeTaskStore) CompleteStepRun(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	loc, ok := f.byInvID[invocationID]
	if !ok {
		return fmt.Errorf("fakes: complete step_run: no such invocation %s", invocationID)
	}
	rows := f.stepRuns[loc.taskID][loc.round]
	existing := rows[loc.idx]
	if err := f.appendEvents([]domain.TaskEvent{
		domain.StepResolvedEvent(taskID, loc.round, existing.StepID, invocationID, run, execErr, time.Since(f.launched[invocationID])),
	}); err != nil {
		return err
	}
	if execErr != nil {
		existing.Status = domain.StepRunStatusFailed
	} else {
		existing.Status = domain.StepRunStatusCompleted
		existing.ExitCode = run.ExitCode
		existing.HasFinding = run.HasFinding
		existing.StructuredResult = run.StructuredResult
	}
	rows[loc.idx] = existing
	return nil
}

func (f *FakeTaskStore) ListStepRuns(ctx context.Context, taskID string, round int) ([]domain.StepRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rows := f.stepRuns[taskID][round]
	out := make([]domain.StepRun, len(rows))
	copy(out, rows)
	return out, nil
}

func (f *FakeTaskStore) ListStepRunsForTask(ctx context.Context, taskID string) ([]domain.StepRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()

	rounds := make([]int, 0, len(f.stepRuns[taskID]))
	for round := range f.stepRuns[taskID] {
		rounds = append(rounds, round)
	}
	sort.Ints(rounds)

	var out []domain.StepRun
	for _, round := range rounds {
		out = append(out, f.stepRuns[taskID][round]...)
	}
	return out, nil
}

func (f *FakeTaskStore) FindWaitingApprovalTask(ctx context.Context, targetRepo string, issueNumber int) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	for id, rec := range f.tasks {
		if rec.Task.TargetRepo == targetRepo && rec.State.Status == domain.TaskStatusWaitingApproval &&
			rec.State.ApprovalIssueNumber != nil && *rec.State.ApprovalIssueNumber == issueNumber {
			return id, true, nil
		}
	}
	return "", false, nil
}

func (f *FakeTaskStore) ListRunningTaskIDs(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	var ids []string
	for id, rec := range f.tasks {
		if rec.State.Status == domain.TaskStatusRunning {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
