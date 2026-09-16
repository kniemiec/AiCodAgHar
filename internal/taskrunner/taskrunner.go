package taskrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/audit"
	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
)

// Dependencies are what a Runner needs: coreloop's own dependencies, the
// TaskStore to persist against, and a direct ContainerLauncher reference —
// separate from coreloop.Dependencies.Executors, which wraps it inside
// AgentStageExecutor/DeterministicStepExecutor without exposing it —
// needed only to re-query a still-"launched" step during startup
// reconciliation (design §4.2).
type Dependencies struct {
	Coreloop coreloop.Dependencies
	Store    TaskStore
	Launcher launcherclient.ContainerLauncher
}

// Result is a task's outcome across every round it ran, in the same shape
// coreloop.Result used to be before this package took over looping. Err is
// set on an infrastructure failure (PDP unreachable, persistence failure,
// ...) — distinct from a normal stop-escalate outcome — mirroring how
// coreloop.RunRound itself distinguishes a returned error from a
// RoundResult.
type Result struct {
	TaskID    string
	Outcome   coreloop.Outcome
	RoundsRun int
	Reason    string
	StepRuns  []domain.StepRun
	Err       error
}

// Runner drives one or more tasks through internal/coreloop.RunRound,
// persisting state after every round so a task survives a control-plane
// restart at any point in its execution (design §4.2).
type Runner struct {
	deps Dependencies
}

// NewRunner builds a Runner. deps.Coreloop.Recorder is overwritten with an
// adapter over deps.Store — callers don't need to wire it themselves.
func NewRunner(deps Dependencies) *Runner {
	deps.Coreloop.Recorder = stepRunRecorder{store: deps.Store}
	return &Runner{deps: deps}
}

// Start inserts task's row (status=running, current_round=0), kicks off
// round 1 in a goroutine, and returns immediately. The caller receives a
// channel it can block on for the task's terminal Result — design §4.2's
// literal taskrunner.Start returns just (taskID, error); this adds the
// channel the design's own prose says the (still-synchronous, until §4.3)
// HTTP handler needs to block on, rather than inventing a separate
// Start/Wait pair.
func (r *Runner) Start(ctx context.Context, task domain.Task) (taskID string, done <-chan Result, err error) {
	if err := r.deps.Store.CreateTask(ctx, task); err != nil {
		return "", nil, fmt.Errorf("taskrunner: start %s: %w", task.ID, err)
	}
	ch := make(chan Result, 1)
	// Deliberately context.Background(), not ctx: a task's execution must
	// outlive the HTTP request that started it (the whole point of this
	// design) — its lifetime is governed by taskrunner's own loop, not the
	// caller's request context.
	go r.runLoop(context.Background(), task, 1, nil, nil, ch)
	return task.ID, ch, nil
}

// Resume reloads a task's persisted state and continues it from its next
// round. Design §4.4's webhook handler is meant to call this in its own
// goroutine after verifying an approval — so Resume itself blocks until
// the task reaches a terminal outcome (or errors), rather than spawning
// another background goroutine of its own.
func (r *Runner) Resume(ctx context.Context, taskID string) (Result, error) {
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, fmt.Errorf("taskrunner: resume %s: load task: %w", taskID, err)
	}
	if !ok {
		return Result{}, fmt.Errorf("taskrunner: resume %s: no such task", taskID)
	}

	done := make(chan Result, 1)
	r.runLoop(ctx, rec.Task, rec.State.CurrentRound+1, rec.State.PriorFindings, nil, done)
	return <-done, nil
}

// runLoop drives rounds, starting at startRound, until a terminal outcome
// or the round cap, persisting state after every round and sending exactly
// one Result to done before returning. alreadyCompleted (only ever
// non-nil from Reconcile) is consumed on the first round only — a
// reconciled round's remaining steps, if any, are dispatched normally, and
// every subsequent round starts fresh.
func (r *Runner) runLoop(ctx context.Context, task domain.Task, startRound int, priorFindings []byte, alreadyCompleted map[string]domain.StepRun, done chan<- Result) {
	defer close(done)

	var allRuns []domain.StepRun
	for round := startRound; round <= coreloop.MaxRounds; round++ {
		if err := r.deps.Store.EnsureRound(ctx, task.ID, round); err != nil {
			done <- Result{TaskID: task.ID, Err: fmt.Errorf("taskrunner: ensure round %d: %w", round, err)}
			return
		}

		res, err := coreloop.RunRound(ctx, r.deps.Coreloop, task, round, priorFindings, alreadyCompleted)
		alreadyCompleted = nil // only ever applies to the round it was built for
		if err != nil {
			done <- Result{TaskID: task.ID, Err: fmt.Errorf("taskrunner: round %d: %w", round, err)}
			return
		}
		allRuns = append(allRuns, res.StepRuns...)

		switch res.Outcome {
		case coreloop.OutcomeContinue:
			priorFindings = res.PriorFindings
			state := domain.TaskState{
				TaskID: task.ID, Status: domain.TaskStatusRunning, CurrentRound: round,
				PriorFindings: priorFindings, UpdatedAt: time.Now(),
			}
			if err := r.deps.Store.UpdateTaskState(ctx, state); err != nil {
				done <- Result{TaskID: task.ID, Err: fmt.Errorf("taskrunner: persist round %d state: %w", round, err)}
				return
			}
			continue

		case coreloop.OutcomeStopSuccess, coreloop.OutcomeStopEscalate:
			result := Result{TaskID: task.ID, Outcome: res.Outcome, RoundsRun: round, Reason: res.Reason, StepRuns: allRuns}
			if err := r.finish(ctx, task, round, result); err != nil {
				done <- Result{TaskID: task.ID, Err: err}
				return
			}
			done <- result
			return

		default:
			done <- Result{TaskID: task.ID, Err: fmt.Errorf("taskrunner: round %d: unexpected round outcome %q", round, res.Outcome)}
			return
		}
	}

	// Round cap exceeded with blocking findings still outstanding — not a
	// RunRound call, so it must persist/audit its own terminal outcome.
	result := Result{
		TaskID: task.ID, Outcome: coreloop.OutcomeStopEscalate, RoundsRun: coreloop.MaxRounds,
		Reason: "round cap reached with blocking findings outstanding", StepRuns: allRuns,
	}
	if err := r.finish(ctx, task, coreloop.MaxRounds, result); err != nil {
		done <- Result{TaskID: task.ID, Err: err}
		return
	}
	done <- result
}

// finish persists a task's terminal state and writes its "task-outcome"
// audit row — the multi-round-scoped equivalent of what coreloop.Run used
// to do internally before this package took over looping.
func (r *Runner) finish(ctx context.Context, task domain.Task, round int, result Result) error {
	status := domain.TaskStatusStopSuccess
	if result.Outcome == coreloop.OutcomeStopEscalate {
		status = domain.TaskStatusStopEscalate
	}
	state := domain.TaskState{TaskID: task.ID, Status: status, CurrentRound: round, Reason: result.Reason, UpdatedAt: time.Now()}
	if err := r.deps.Store.UpdateTaskState(ctx, state); err != nil {
		return fmt.Errorf("taskrunner: persist terminal state for %s: %w", task.ID, err)
	}
	if r.deps.Coreloop.Audit == nil {
		return nil
	}
	return r.auditTaskOutcome(ctx, task, result)
}

// auditTaskOutcome writes one "task-outcome" row per task — the terminal
// stop-success/stop-escalate decision point ADR-1's execution-outcome
// merge otherwise has nowhere to land, since it isn't scoped to any single
// invocation. Moved here from coreloop verbatim: it's task-scoped, and
// coreloop no longer has a task-scoped concept now that RunRound only
// knows about one round.
func (r *Runner) auditTaskOutcome(ctx context.Context, task domain.Task, result Result) error {
	return r.deps.Coreloop.Audit.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
		InvocationID: fmt.Sprintf("%s:outcome", task.ID),
		TaskID:       task.ID,
		Round:        result.RoundsRun,
		StepType:     "task-outcome",
		BotAccount:   task.BotAccount,
		TargetRepo:   task.TargetRepo,
		Status:       string(result.Outcome),
		Evidence:     result.Reason,
	})
}

// Reconcile scans every status=running task and resumes each one from
// wherever it left off, in its own goroutine — the startup-reconciliation
// half of design §4.2, meant to be called once from control-plane's
// startup path. It does not block on any task completing; errors
// reconciling an individual task are logged, not returned, since one bad
// task shouldn't block the rest or startup itself.
func (r *Runner) Reconcile(ctx context.Context) error {
	ids, err := r.deps.Store.ListRunningTaskIDs(ctx)
	if err != nil {
		return fmt.Errorf("taskrunner: reconcile: list running tasks: %w", err)
	}
	for _, id := range ids {
		go func(id string) {
			if _, err := r.ReconcileTask(context.Background(), id); err != nil {
				log.Printf("taskrunner: reconcile %s: %v", id, err)
			}
		}(id)
	}
	return nil
}

// ReconcileTask inspects one running task's current round's step_runs and
// either resumes it (dispatching whatever's missing, skipping whatever's
// already completed) or, if a still-"launched" step can't be resolved
// against the launcher, escalates the task rather than guessing — "don't
// blindly relaunch" (design §4.2). Exported (rather than folded into
// Reconcile as an unexported per-task helper) so it's independently
// callable and synchronous — Reconcile's only job on top of this is fan-out
// across every running task.
//
// A task that isn't status=running (already finished, or simply doesn't
// exist) is a no-op, not an error — Reconcile's caller only has a snapshot
// list of IDs, and reconciling twice, or after a task has already reached a
// terminal state some other way, must be safe.
func (r *Runner) ReconcileTask(ctx context.Context, taskID string) (Result, error) {
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, fmt.Errorf("taskrunner: reconcile %s: load task: %w", taskID, err)
	}
	if !ok || rec.State.Status != domain.TaskStatusRunning {
		return Result{TaskID: taskID}, nil
	}

	// CurrentRound records the last *completed* round (0 if none has
	// completed yet), so resuming — like Resume — means the round after
	// it. Using CurrentRound as-is would re-inspect an already-finished
	// round's step_runs (all "completed") while silently skipping the
	// round actually in flight when the crash happened, leading to either
	// a duplicate "task-outcome" audit write or a fresh, blind re-dispatch
	// of that in-flight round's agent work.
	round := rec.State.CurrentRound + 1

	stepRuns, err := r.deps.Store.ListStepRuns(ctx, taskID, round)
	if err != nil {
		return Result{}, fmt.Errorf("taskrunner: reconcile %s round %d: list step_runs: %w", taskID, round, err)
	}

	alreadyCompleted := map[string]domain.StepRun{}
	for _, sr := range stepRuns {
		switch sr.Status {
		case domain.StepRunStatusCompleted:
			alreadyCompleted[sr.StepID] = sr

		case domain.StepRunStatusLaunched:
			resolved, ok := r.resolveLaunchedStep(ctx, taskID, sr)
			if !ok {
				reason := fmt.Sprintf(
					"round %d: step %s (invocation %s) was still 'launched' at startup and could not be reconciled with the launcher",
					round, sr.StepID, sr.InvocationID,
				)
				result := Result{TaskID: taskID, Outcome: coreloop.OutcomeStopEscalate, RoundsRun: round, Reason: reason}
				if err := r.finish(ctx, rec.Task, round, result); err != nil {
					return Result{}, fmt.Errorf("taskrunner: reconcile %s: %w", taskID, err)
				}
				return result, nil
			}
			alreadyCompleted[sr.StepID] = resolved

		case domain.StepRunStatusFailed:
			// Safe to retry: leave it out of alreadyCompleted so RunRound
			// redispatches it.
		}
	}

	done := make(chan Result, 1)
	r.runLoop(ctx, rec.Task, round, rec.State.PriorFindings, alreadyCompleted, done)
	result := <-done
	return result, result.Err
}

// resolveLaunchedStep re-queries a still-"launched" step's real outcome.
// The launcher gRPC contract (api/proto/launcher/v1/launcher.proto) has no
// status-query-by-invocation-id RPC — only Launch (synchronous), Kill, and
// ExtractLogs — so this reuses ExtractLogs, which its own doc comment
// already describes as "independently callable, decoupled from Launch".
// That's sufficient to recover an agent-stage step's has_blocking_finding
// signal, but LogBundle carries no exit code, so a resolved
// deterministic-step's HasFinding is approximated as false here — a
// documented mocked-world simplification (matching this codebase's
// existing "Phase N stub" stubs), not a claim that's exact for a real,
// non-mocked launcher.
func (r *Runner) resolveLaunchedStep(ctx context.Context, taskID string, sr domain.StepRun) (domain.StepRun, bool) {
	logs, err := r.deps.Launcher.ExtractLogs(ctx, sr.InvocationID)
	if err != nil {
		return domain.StepRun{}, false
	}

	var parsed struct {
		HasBlockingFinding bool `json:"has_blocking_finding"`
	}
	if len(logs.StructuredResultJSON) > 0 {
		_ = json.Unmarshal(logs.StructuredResultJSON, &parsed)
	}

	resolved := domain.StepRun{
		Round: sr.Round, StepID: sr.StepID, StepType: sr.StepType, InvocationID: sr.InvocationID,
		Blocking: sr.Blocking, HasFinding: parsed.HasBlockingFinding, StructuredResult: logs.StructuredResultJSON,
		Status: domain.StepRunStatusCompleted,
	}
	if err := r.deps.Store.CompleteStepRun(ctx, taskID, sr.InvocationID, resolved, nil); err != nil {
		log.Printf("taskrunner: reconcile %s: persist resolved step %s: %v", taskID, sr.StepID, err)
	}
	return resolved, true
}
