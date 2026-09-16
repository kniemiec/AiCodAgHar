package fakes

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
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
}

func (f *FakeTaskStore) init() {
	if f.tasks == nil {
		f.tasks = map[string]domain.TaskRecord{}
		f.rounds = map[string]map[int]bool{}
		f.stepRuns = map[string]map[int][]domain.StepRun{}
		f.byInvID = map[string]stepRunLocation{}
	}
}

func (f *FakeTaskStore) CreateTask(ctx context.Context, task domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if _, ok := f.tasks[task.ID]; ok {
		return fmt.Errorf("fakes: task %s already exists", task.ID)
	}
	f.tasks[task.ID] = domain.TaskRecord{
		Task:  task,
		State: domain.TaskState{TaskID: task.ID, Status: domain.TaskStatusRunning, CurrentRound: 0},
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

func (f *FakeTaskStore) UpdateTaskState(ctx context.Context, state domain.TaskState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	rec, ok := f.tasks[state.TaskID]
	if !ok {
		return fmt.Errorf("fakes: update task state: no such task %s", state.TaskID)
	}
	rec.State = state
	f.tasks[state.TaskID] = rec
	return nil
}

func (f *FakeTaskStore) EnsureRound(ctx context.Context, taskID string, round int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.rounds[taskID] == nil {
		f.rounds[taskID] = map[int]bool{}
	}
	f.rounds[taskID][round] = true
	return nil
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
