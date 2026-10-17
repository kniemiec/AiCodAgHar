package taskrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
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

	// MaxRetries bounds operator retries per task (TASK_MAX_RETRIES). Zero
	// means DefaultMaxRetries.
	MaxRetries int

	// Metrics receives the runner's own counts that no event carries
	// (reconcile outcomes, failures it could not persist). Nil is a no-op.
	Metrics RunnerMetrics
}

// RunnerMetrics is Dependencies.Metrics' interface (design §6: MT-10, ER-5).
type RunnerMetrics interface {
	// Reconciled counts one ReconcileTask call by outcome (Reconcile*).
	Reconciled(ctx context.Context, outcome string)
	// PersistenceFailure counts a task failure that could not be persisted.
	PersistenceFailure(ctx context.Context)
}

// ReconcileTask outcomes for RunnerMetrics.Reconciled.
const (
	ReconcileResumed = "resumed"
	ReconcileFailed  = "failed" // a launched step could not be reconciled
	ReconcileNoop    = "noop"   // not running (finished, failed, or unknown)
	ReconcileError   = "error"
)

func (r *Runner) reconciled(ctx context.Context, outcome string) {
	if r.deps.Metrics != nil {
		r.deps.Metrics.Reconciled(ctx, outcome)
	}
}

// DefaultMaxRetries is the operator-retry bound when Dependencies.MaxRetries
// is unset.
const DefaultMaxRetries = 3

// ErrNotRetryable is returned by Retry/StartRetry for a task that is not a
// retryable failed task with retries left.
var ErrNotRetryable = domain.ErrNotRetryable

// ErrNoSuchTask is returned by Retry/StartRetry for an unknown task ID.
var ErrNoSuchTask = errors.New("no such task")

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
	// ReasonKind classifies a stop-escalate Reason (domain.Reason*); empty
	// for success and for failures.
	ReasonKind string
	StepRuns   []domain.StepRun
	Err        error
}

// Runner drives one or more tasks through internal/control-plane/coreloop.RunRound,
// persisting state after every round so a task survives a control-plane
// restart at any point in its execution (design §4.2).
type Runner struct {
	deps Dependencies
}

// NewRunner builds a Runner. deps.Coreloop.Recorder and deps.Coreloop.
// PRRecorder are overwritten with deps.Store (TaskStore satisfies both
// narrow interfaces structurally) — callers don't need to wire them
// themselves.
func NewRunner(deps Dependencies) *Runner {
	deps.Coreloop.Recorder = stepRunRecorder{store: deps.Store}
	deps.Coreloop.PRRecorder = deps.Store
	deps.Coreloop.Events = deps.Store
	if deps.MaxRetries <= 0 {
		deps.MaxRetries = DefaultMaxRetries
	}
	return &Runner{deps: deps}
}

// Start inserts task's row (status=running, current_round=0), kicks off
// round 1 in a goroutine, and returns immediately. The caller receives a
// channel it can block on for the task's terminal Result — design §4.2's
// literal taskrunner.Start returns just (taskID, error); this adds the
// channel that let the (pre-§4.3) HTTP handler stay synchronous for a
// transitional period rather than inventing a separate Start/Wait pair.
// Now that §4.3's handler replies 202 without waiting, the channel's only
// consumers are tests wanting a deterministic way to await completion
// instead of polling GetTaskStatus.
func (r *Runner) Start(ctx context.Context, task domain.Task) (taskID string, done <-chan Result, err error) {
	if err := r.deps.Store.CreateTask(ctx, task); err != nil {
		return "", nil, fmt.Errorf("taskrunner: start %s: %w", task.ID, err)
	}
	slog.InfoContext(logging.WithTask(ctx, task.ID), "task accepted", "target_repo", task.TargetRepo, "base_branch", task.BaseBranch,
		"branch", task.Branch, "risk_tier", task.RiskTier, "bot_account", task.BotAccount, "triggering_principal", task.TriggeringPrincipal)
	ch := make(chan Result, 1)
	// Deliberately context.Background(), not ctx: a task's execution must
	// outlive the HTTP request that started it (the whole point of this
	// design) — its lifetime is governed by taskrunner's own loop, not the
	// caller's request context.
	go r.runLoop(context.Background(), task, 1, nil, nil, "", ch)
	return task.ID, ch, nil
}

// Resume reloads a task's persisted state and continues it from its next
// round — for an approve-phase pause specifically (design §4.4), that
// "next round" is the very round that paused, re-evaluated fresh:
// loadAlreadyCompleted means its generate/review steps aren't redispatched,
// only whatever the approve gate was blocking (publish, typically) actually
// runs. Design §4.4's webhook handler is meant to call this in its own
// goroutine after verifying an approval — so Resume itself blocks until the
// task reaches a terminal outcome (or errors), rather than spawning another
// background goroutine of its own.
func (r *Runner) Resume(ctx context.Context, taskID string) (Result, error) {
	ctx = logging.WithTask(ctx, taskID)
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, fmt.Errorf("taskrunner: resume %s: load task: %w", taskID, err)
	}
	if !ok {
		return Result{}, fmt.Errorf("taskrunner: resume %s: no such task", taskID)
	}

	round := rec.State.CurrentRound + 1
	done := make(chan Result, 1)
	alreadyCompleted, err := r.loadAlreadyCompleted(ctx, rec.Task, round)
	if err != nil {
		if r.failUnreconcilable(ctx, rec.Task, round, err, done) {
			return <-done, nil
		}
		return Result{}, fmt.Errorf("taskrunner: resume %s: %w", taskID, err)
	}
	r.runLoop(ctx, rec.Task, round, rec.State.PriorFindings, alreadyCompleted, domain.RoundResumeApproval, done)
	return <-done, nil
}

// runLoop drives rounds, starting at startRound, until a terminal outcome
// or the round cap, persisting state after every round and sending exactly
// one Result to done before returning. alreadyCompleted (only ever
// non-nil from Reconcile) is consumed on the first round only — a
// reconciled round's remaining steps, if any, are dispatched normally, and
// every subsequent round starts fresh.
func (r *Runner) runLoop(ctx context.Context, task domain.Task, startRound int, priorFindings []byte, alreadyCompleted map[string]domain.StepRun, resumeReason string, done chan<- Result) {
	defer close(done)
	ctx = logging.WithTask(ctx, task.ID)

	var allRuns []domain.StepRun
	for round := startRound; round <= coreloop.MaxRounds; round++ {
		ctx = logging.WithRound(ctx, round)
		slog.InfoContext(ctx, "round started", "pr_number", task.PRNumber, "reconciled_steps", len(alreadyCompleted))
		created, err := r.deps.Store.EnsureRound(ctx, task.ID, round)
		if err != nil {
			r.fail(ctx, task, round, fmt.Errorf("taskrunner: ensure round %d: %w", round, err), done)
			return
		}
		if !created && resumeReason != "" {
			if err := r.deps.Store.AppendEvents(ctx, domain.TaskEvent{TaskID: task.ID, Round: round, Type: domain.EventRoundResumed,
				Payload: map[string]any{"reason": resumeReason}}); err != nil {
				r.fail(ctx, task, round, fmt.Errorf("taskrunner: record round %d resumed: %w", round, err), done)
				return
			}
		}
		resumeReason = "" // only the first round of a loop is a resumption

		res, err := coreloop.RunRound(ctx, r.deps.Coreloop, task, round, priorFindings, alreadyCompleted)
		alreadyCompleted = nil // only ever applies to the round it was built for
		if err != nil {
			r.fail(ctx, task, round, fmt.Errorf("taskrunner: round %d: %w", round, err), done)
			return
		}
		allRuns = append(allRuns, res.StepRuns...)
		slog.InfoContext(ctx, "round finished",
			"outcome", res.Outcome, "reason", res.Reason, "pr_number", res.PRNumber, "step_runs", len(res.StepRuns))
		// Task Intent Propagation design §5: RunRound may have just opened
		// the review PR (task.PRNumber was 0 going in) — carry the result
		// forward into task so a subsequent round in this same loop doesn't
		// see PRNumber==0 again and try to reopen it. RunRound itself only
		// ever sees a copy of task, so this is the one place that mutation
		// needs to be threaded back in.
		task.PRNumber = res.PRNumber

		switch res.Outcome {
		case coreloop.OutcomeContinue:
			priorFindings = res.PriorFindings
			state := domain.TaskState{
				TaskID: task.ID, Status: domain.TaskStatusRunning, CurrentRound: round,
				PriorFindings: priorFindings, UpdatedAt: time.Now(),
			}
			if err := r.deps.Store.UpdateTaskState(ctx, state, roundFinishedEvent(task.ID, round, res)); err != nil {
				r.fail(ctx, task, round, fmt.Errorf("taskrunner: persist round %d state: %w", round, err), done)
				return
			}
			continue

		case coreloop.OutcomeWaitingApproval:
			// This round hasn't completed — CurrentRound stays at round-1
			// (design §4.2's "last completed round" invariant, which Resume
			// and ReconcileTask both rely on via +1 to find their way back
			// here) and priorFindings is left untouched, since it's still
			// this round's own input, not a new round's.
			issueNum := res.ApprovalIssueNumber
			state := domain.TaskState{
				TaskID: task.ID, Status: domain.TaskStatusWaitingApproval, CurrentRound: round - 1,
				ApprovalIssueNumber: &issueNum, PriorFindings: priorFindings, UpdatedAt: time.Now(),
			}
			requested := domain.TaskEvent{TaskID: task.ID, Round: round, Type: domain.EventApprovalRequested, Payload: map[string]any{"issue_number": issueNum}}
			if err := r.deps.Store.UpdateTaskState(ctx, state, requested); err != nil {
				r.fail(ctx, task, round, fmt.Errorf("taskrunner: persist waiting_approval state for round %d: %w", round, err), done)
				return
			}
			done <- Result{TaskID: task.ID, Outcome: res.Outcome, RoundsRun: round, StepRuns: allRuns}
			return

		case coreloop.OutcomeStopSuccess, coreloop.OutcomeStopEscalate:
			result := Result{TaskID: task.ID, Outcome: res.Outcome, RoundsRun: round, Reason: res.Reason, ReasonKind: res.ReasonKind, StepRuns: allRuns}
			if err := r.finish(ctx, task, round, result, nil, roundFinishedEvent(task.ID, round, res)); err != nil {
				r.fail(ctx, task, round, err, done)
				return
			}
			done <- result
			return

		default:
			r.fail(ctx, task, round, fmt.Errorf("taskrunner: round %d: unexpected round outcome %q", round, res.Outcome), done)
			return
		}
	}

	slog.WarnContext(ctx, "round cap reached with blocking findings outstanding; escalating", "max_rounds", coreloop.MaxRounds)
	// Round cap exceeded with blocking findings still outstanding — not a
	// RunRound call, so it must persist/audit its own terminal outcome.
	result := Result{
		TaskID: task.ID, Outcome: coreloop.OutcomeStopEscalate, RoundsRun: coreloop.MaxRounds,
		Reason: "round cap reached with blocking findings outstanding", ReasonKind: domain.ReasonRoundCap, StepRuns: allRuns,
	}
	if err := r.finish(ctx, task, coreloop.MaxRounds, result, nil); err != nil {
		r.fail(ctx, task, coreloop.MaxRounds, err, done)
		return
	}
	done <- result
}

// fail ends runLoop on an error: the task moves to status=failed with the
// error's class and retryability (design §2), one task.failed event in the
// same transaction, and an error-level log line. A retryable failure waits
// for an operator retry; a restart never resumes a failed task. If the
// failure itself cannot be persisted (Postgres down) the task stays running —
// restart reconciliation recovers it — and the write failure is logged.
func (r *Runner) fail(ctx context.Context, task domain.Task, round int, err error, done chan<- Result) {
	class, retryable, dep := failure.Classify(err)
	ctx = logging.WithRound(ctx, round)
	slog.ErrorContext(ctx, "task failed",
		"error_class", class, "retryable", retryable, "dependency", dep, "error", err)

	reason := failure.Bound(err.Error())
	payload := map[string]any{"error_class": string(class), "retryable": retryable, "error": reason, "target_repo": task.TargetRepo}
	if dep != "" {
		payload["dependency"] = dep
	}
	if !retryable {
		if rec, ok, gerr := r.deps.Store.GetTask(ctx, task.ID); gerr == nil && ok && !rec.State.AcceptedAt.IsZero() {
			payload["wall_ms"] = time.Since(rec.State.AcceptedAt).Milliseconds()
		}
	}
	ev := domain.TaskEvent{TaskID: task.ID, Round: round, Type: domain.EventTaskFailed, Payload: payload}
	rec := domain.FailureRecord{TaskID: task.ID, Round: round, Class: string(class), Retryable: retryable, Reason: reason}
	if serr := r.deps.Store.FailTask(ctx, rec, ev); serr != nil {
		slog.ErrorContext(ctx, "could not persist task failure; task left running for reconciliation", "error", serr)
		if r.deps.Metrics != nil {
			r.deps.Metrics.PersistenceFailure(ctx)
		}
	}
	if done != nil {
		done <- Result{TaskID: task.ID, Err: err}
	}
}

// failUnreconcilable fails the task when err says a launched step could not be
// reconciled with the launcher (a retryable dependency failure — not an
// audited escalation any more, design §2). It reports whether err was that
// case; any other load error is left to the caller, with no state change.
func (r *Runner) failUnreconcilable(ctx context.Context, task domain.Task, round int, err error, done chan<- Result) bool {
	if !errors.Is(err, errUnreconcilable) {
		return false
	}
	r.fail(ctx, task, round, err, done)
	return true
}

// errUnreconcilable marks loadAlreadyCompleted's "launched step could not be
// resolved against the launcher" error.
var errUnreconcilable = errors.New("launched step could not be reconciled with the launcher")

// roundFinishedEvent describes a round that ran to completion (continue or
// stop-*); a waiting-approval round hasn't finished and never gets one.
func roundFinishedEvent(taskID string, round int, res coreloop.RoundResult) domain.TaskEvent {
	blockingSteps := 0
	for _, sr := range res.StepRuns {
		if sr.Blocking && sr.HasFinding {
			blockingSteps++
		}
	}
	return domain.TaskEvent{TaskID: taskID, Round: round, Type: domain.EventRoundFinished, Payload: map[string]any{
		"outcome": string(res.Outcome), "blocking_findings": blockingSteps > 0, "blocking_steps": blockingSteps,
	}}
}

// approvalEvidence carries an approve-phase gate's real verification outcome
// (design §4.4) into the terminal task-outcome audit row — nil for every
// other terminal path (round cap, PDP deny, a review round's own blocking
// findings persisting to the cap, ...).
type approvalEvidence struct {
	Approved   bool
	ApprovedBy string
	Evidence   string
}

// finish persists a task's terminal state and writes its "task-outcome"
// audit row — the multi-round-scoped equivalent of what coreloop.Run used
// to do internally before this package took over looping. approvalOutcome is
// non-nil only when a rejection is what produced this terminal result
// (RecordRejection); every other caller passes nil.
func (r *Runner) finish(ctx context.Context, task domain.Task, round int, result Result, approvalOutcome *approvalEvidence, preceding ...domain.TaskEvent) error {
	status := domain.TaskStatusStopSuccess
	outcome := "success"
	if result.Outcome == coreloop.OutcomeStopEscalate {
		status = domain.TaskStatusStopEscalate
		outcome = "escalated"
	}

	// wall/wait/active (design §4) come from the committed events plus the
	// ones this very write adds, which aren't visible yet.
	now := time.Now()
	history, err := r.deps.Store.ListEvents(ctx, task.ID, domain.EventFilter{})
	if err != nil {
		return fmt.Errorf("taskrunner: load events for %s: %w", task.ID, err)
	}
	for _, e := range preceding {
		e.At = now
		history = append(history, e)
	}
	wall, wait, _ := domain.WaitActive(history, now)
	finished := domain.TaskEvent{TaskID: task.ID, Round: round, Type: domain.EventTaskFinished, Payload: map[string]any{
		"outcome": outcome, "reason_kind": result.ReasonKind, "rounds": round, "target_repo": task.TargetRepo,
		"wall_ms": wall.Milliseconds(), "wait_ms": wait.Milliseconds(),
	}}

	state := domain.TaskState{TaskID: task.ID, Status: status, CurrentRound: round, Reason: result.Reason, UpdatedAt: now}
	if err := r.deps.Store.UpdateTaskState(ctx, state, append(preceding, finished)...); err != nil {
		return fmt.Errorf("taskrunner: persist terminal state for %s: %w", task.ID, err)
	}
	if r.deps.Coreloop.Audit == nil {
		return nil
	}
	return r.auditTaskOutcome(ctx, task, result, approvalOutcome)
}

// auditTaskOutcome writes one "task-outcome" row per task — the terminal
// stop-success/stop-escalate decision point ADR-1's execution-outcome
// merge otherwise has nowhere to land, since it isn't scoped to any single
// invocation. Moved here from coreloop verbatim: it's task-scoped, and
// coreloop no longer has a task-scoped concept now that RunRound only
// knows about one round.
func (r *Runner) auditTaskOutcome(ctx context.Context, task domain.Task, result Result, approvalOutcome *approvalEvidence) error {
	rec := audit.InvocationAuditRecord{
		InvocationID: fmt.Sprintf("%s:outcome", task.ID),
		TaskID:       task.ID,
		Round:        result.RoundsRun,
		StepType:     "task-outcome",
		BotAccount:   task.BotAccount,
		TargetRepo:   task.TargetRepo,
		Status:       string(result.Outcome),
		Evidence:     result.Reason,
	}
	if approvalOutcome != nil {
		rec.Approved = approvalOutcome.Approved
		rec.ApprovedBy = approvalOutcome.ApprovedBy
		rec.Evidence = approvalOutcome.Evidence
	}
	return r.deps.Coreloop.Audit.WriteInvocationRecord(ctx, rec)
}

// auditApprovalGranted records the approve-phase gate's own verification
// event (design §4.4) — separate from, and earlier than, whatever
// task-outcome row the task eventually reaches once Resume finishes it,
// since granting approval doesn't itself terminate the task.
func (r *Runner) auditApprovalGranted(ctx context.Context, task domain.Task, round int, approvedBy, evidence string) error {
	if r.deps.Coreloop.Audit == nil {
		return nil
	}
	return r.deps.Coreloop.Audit.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
		InvocationID: fmt.Sprintf("%s:round-%d:approval", task.ID, round),
		TaskID:       task.ID,
		Round:        round,
		StepType:     "approval",
		BotAccount:   task.BotAccount,
		TargetRepo:   task.TargetRepo,
		Status:       "approved",
		Approved:     true,
		ApprovedBy:   approvedBy,
		Evidence:     evidence,
	})
}

// RecordApproval persists a verified approval (design §4.4 step 5) —
// approvedBy and evidence come from the webhook handler's own independent
// verification (HMAC signature, APPROVERS-file membership, live
// collaborator-role check); RecordApproval itself trusts that work rather
// than redoing it. It flips status back to running so the task is eligible
// for Resume, but does not call Resume itself — the caller dispatches that
// separately, in its own goroutine, so the webhook response returns fast.
func (r *Runner) RecordApproval(ctx context.Context, taskID, approvedBy, evidence string) error {
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("taskrunner: record approval for %s: load task: %w", taskID, err)
	}
	if !ok || rec.State.Status != domain.TaskStatusWaitingApproval {
		return fmt.Errorf("taskrunner: record approval for %s: not waiting_approval", taskID)
	}

	state := domain.TaskState{
		TaskID: taskID, Status: domain.TaskStatusRunning, CurrentRound: rec.State.CurrentRound,
		ApprovalIssueNumber: rec.State.ApprovalIssueNumber, ApprovedBy: approvedBy,
		PriorFindings: rec.State.PriorFindings, UpdatedAt: time.Now(),
	}
	granted := domain.TaskEvent{TaskID: taskID, Round: rec.State.CurrentRound + 1, Type: domain.EventApprovalGranted,
		Payload: map[string]any{"by": approvedBy, "wait_ms": r.approvalWait(ctx, taskID).Milliseconds()}}
	if err := r.deps.Store.UpdateTaskState(ctx, state, granted); err != nil {
		return fmt.Errorf("taskrunner: record approval for %s: %w", taskID, err)
	}
	return r.auditApprovalGranted(ctx, rec.Task, rec.State.CurrentRound+1, approvedBy, evidence)
}

// RecordRejection persists a verified rejection (design §4.4 step 5) as the
// task's terminal outcome — unlike an approval, a rejection never resumes
// anything, so this is the whole of the webhook handler's rejected path.
func (r *Runner) RecordRejection(ctx context.Context, taskID, rejectedBy, evidence string) error {
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("taskrunner: record rejection for %s: load task: %w", taskID, err)
	}
	if !ok || rec.State.Status != domain.TaskStatusWaitingApproval {
		return fmt.Errorf("taskrunner: record rejection for %s: not waiting_approval", taskID)
	}

	round := rec.State.CurrentRound + 1
	result := Result{
		TaskID: taskID, Outcome: coreloop.OutcomeStopEscalate, RoundsRun: round,
		Reason: fmt.Sprintf("approval rejected by %s", rejectedBy), ReasonKind: domain.ReasonApprovalRejected,
	}
	rejected := domain.TaskEvent{TaskID: taskID, Round: round, Type: domain.EventApprovalRejected,
		Payload: map[string]any{"by": rejectedBy, "wait_ms": r.approvalWait(ctx, taskID).Milliseconds()}}
	return r.finish(ctx, rec.Task, round, result, &approvalEvidence{Approved: false, ApprovedBy: rejectedBy, Evidence: evidence}, rejected)
}

// approvalWait is how long the task has waited since its latest
// approval.requested (MT-7); zero when that cannot be read, since the wait
// is reporting, not a reason to refuse the approval.
func (r *Runner) approvalWait(ctx context.Context, taskID string) time.Duration {
	events, err := r.deps.Store.ListEvents(ctx, taskID, domain.EventFilter{Type: domain.EventApprovalRequested})
	if err != nil || len(events) == 0 {
		return 0
	}
	return time.Since(events[len(events)-1].At)
}

// TaskStatus is a task's current, queryable state — GET /tasks/{id}'s
// (design §4.3) view. Composed from TaskStore.GetTask and
// ListStepRunsForTask rather than the HTTP layer reaching into TaskStore
// directly, the same way handleCreateTask never touched it either.
type TaskStatus struct {
	Task     domain.Task
	State    domain.TaskState
	StepRuns []domain.StepRun
}

// GetTaskStatus loads taskID's current record and full step-run history.
// ok is false if no such task exists.
func (r *Runner) GetTaskStatus(ctx context.Context, taskID string) (TaskStatus, bool, error) {
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return TaskStatus{}, false, fmt.Errorf("taskrunner: get status %s: %w", taskID, err)
	}
	if !ok {
		return TaskStatus{}, false, nil
	}
	stepRuns, err := r.deps.Store.ListStepRunsForTask(ctx, taskID)
	if err != nil {
		return TaskStatus{}, false, fmt.Errorf("taskrunner: get status %s: %w", taskID, err)
	}
	return TaskStatus{Task: rec.Task, State: rec.State, StepRuns: stepRuns}, true, nil
}

// TaskEvents returns a task's recorded events (GET /tasks/{id}/events). ok is
// false if no such task exists.
func (r *Runner) TaskEvents(ctx context.Context, taskID string, f domain.EventFilter) (events []domain.TaskEvent, ok bool, err error) {
	if _, ok, err := r.deps.Store.GetTask(ctx, taskID); err != nil || !ok {
		return nil, false, err
	}
	events, err = r.deps.Store.ListEvents(ctx, taskID, f)
	if err != nil {
		return nil, false, fmt.Errorf("taskrunner: events for %s: %w", taskID, err)
	}
	return events, true, nil
}

// TasksByStatus lists tasks currently in status with how long each has been
// there (the admin listing).
func (r *Runner) TasksByStatus(ctx context.Context, status string) ([]domain.TaskAge, error) {
	return r.deps.Store.ListTasksByStatus(ctx, status)
}

// FindWaitingApprovalTask looks up the task currently waiting on
// issueNumber in targetRepo — the webhook handler's entry point (design
// §4.4 step 3), kept behind Runner so cmd/control-plane never touches
// TaskStore directly.
func (r *Runner) FindWaitingApprovalTask(ctx context.Context, targetRepo string, issueNumber int) (taskID string, ok bool, err error) {
	return r.deps.Store.FindWaitingApprovalTask(ctx, targetRepo, issueNumber)
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
	slog.InfoContext(ctx, "startup reconciliation", "running_tasks", len(ids))
	for _, id := range ids {
		go func(id string) {
			ctx := logging.WithTask(context.Background(), id)
			if _, err := r.ReconcileTask(ctx, id); err != nil {
				slog.ErrorContext(ctx, "reconcile task failed", "error", err)
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
	ctx = logging.WithTask(ctx, taskID)
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		r.reconciled(ctx, ReconcileError)
		return Result{}, fmt.Errorf("taskrunner: reconcile %s: load task: %w", taskID, err)
	}
	if !ok || rec.State.Status != domain.TaskStatusRunning {
		r.reconciled(ctx, ReconcileNoop)
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
	done := make(chan Result, 1)
	alreadyCompleted, err := r.loadAlreadyCompleted(ctx, rec.Task, round)
	if err != nil {
		if r.failUnreconcilable(ctx, rec.Task, round, err, done) {
			r.reconciled(ctx, ReconcileFailed)
			result := <-done
			return result, result.Err
		}
		r.reconciled(ctx, ReconcileError)
		return Result{}, fmt.Errorf("taskrunner: reconcile %s: %w", taskID, err)
	}
	if err := r.deps.Store.AppendEvents(ctx, domain.TaskEvent{TaskID: taskID, Round: round, Type: domain.EventTaskReconciled,
		Payload: map[string]any{"from_status": rec.State.Status, "resume_round": round, "completed_steps": len(alreadyCompleted)}}); err != nil {
		r.reconciled(ctx, ReconcileError)
		return Result{}, fmt.Errorf("taskrunner: reconcile %s: %w", taskID, err)
	}
	r.reconciled(ctx, ReconcileResumed)
	r.runLoop(ctx, rec.Task, round, rec.State.PriorFindings, alreadyCompleted, domain.RoundResumeReconcile, done)
	result := <-done
	return result, result.Err
}

// loadAlreadyCompleted builds the alreadyCompleted map a resumed round needs
// so RunRound doesn't blindly redispatch a step it already ran before
// pausing (for an approve-phase gate, design §4.4) or crashing mid-round
// (design §4.2): completed rows are reused as-is, a still-"launched" row is
// resolved against the launcher, and a failed row is left out so RunRound
// retries it. A still-"launched" row that can't be resolved yields an error
// wrapping errUnreconcilable and classed as a retryable launcher-dependency
// failure — the caller fails the task rather than guessing (it used to
// escalate with an audit row; an infrastructure fault is not an outcome of
// authority).
func (r *Runner) loadAlreadyCompleted(ctx context.Context, task domain.Task, round int) (map[string]domain.StepRun, error) {
	stepRuns, err := r.deps.Store.ListStepRuns(ctx, task.ID, round)
	if err != nil {
		return nil, fmt.Errorf("round %d: list step_runs: %w", round, err)
	}

	alreadyCompleted := map[string]domain.StepRun{}
	for _, sr := range stepRuns {
		switch sr.Status {
		case domain.StepRunStatusCompleted:
			alreadyCompleted[sr.StepID] = sr

		case domain.StepRunStatusLaunched:
			resolved, resolvedOK := r.resolveLaunchedStep(ctx, task.ID, sr)
			if !resolvedOK {
				slog.WarnContext(ctx, "launched step could not be reconciled with the launcher; failing task",
					"round", round, "step_id", sr.StepID, "invocation_id", sr.InvocationID)
				return nil, failure.Dependency(failure.DepLauncher, fmt.Errorf(
					"round %d: step %s (invocation %s) was still 'launched' at startup: %w",
					round, sr.StepID, sr.InvocationID, errUnreconcilable))
			}
			alreadyCompleted[sr.StepID] = resolved

		case domain.StepRunStatusFailed:
			// Safe to retry: leave it out of alreadyCompleted so RunRound
			// redispatches it.
		}
	}
	return alreadyCompleted, nil
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
	ctx = logging.WithStep(logging.WithRound(ctx, sr.Round), sr.StepID, sr.InvocationID)
	logs, err := r.deps.Launcher.ExtractLogs(ctx, sr.InvocationID)
	if err != nil {
		slog.WarnContext(ctx, "reconcile: extract logs failed", "error", err)
		r.recordStepReconciled(ctx, taskID, sr, false)
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
		slog.WarnContext(ctx, "reconcile: persist resolved step failed", "error", err)
	}
	r.recordStepReconciled(ctx, taskID, sr, true)
	return resolved, true
}

// recordStepReconciled appends step.reconciled. Best effort: reconciliation
// already decides the task's fate on its own, and the event is only history.
func (r *Runner) recordStepReconciled(ctx context.Context, taskID string, sr domain.StepRun, resolved bool) {
	if err := r.deps.Store.AppendEvents(ctx, domain.TaskEvent{TaskID: taskID, Round: sr.Round, StepID: sr.StepID, InvocationID: sr.InvocationID,
		Type: domain.EventStepReconciled, Payload: map[string]any{"resolved": resolved}}); err != nil {
		slog.WarnContext(ctx, "reconcile: record step.reconciled failed", "error", err)
	}
}

// StartRetry is the operator-retry entry point (design §2): a compare-and-set
// that moves a retryable failed task with retries left back to running and
// records task.retried, then continues it exactly as startup reconciliation
// would — in a goroutine, so the caller (the admin handler) answers 202
// immediately. The Result arrives on the returned channel. ErrNoSuchTask and
// ErrNotRetryable are returned synchronously, before anything changes.
// operator is self-declared (there is no authn yet, N4) and recorded beside
// remoteAddr.
func (r *Runner) StartRetry(ctx context.Context, taskID, operator, remoteAddr string) (<-chan Result, error) {
	ctx = logging.WithTask(ctx, taskID)
	rec, ok, err := r.deps.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("taskrunner: retry %s: load task: %w", taskID, err)
	}
	if !ok {
		return nil, ErrNoSuchTask
	}
	ev := domain.TaskEvent{TaskID: taskID, Round: rec.State.CurrentRound + 1, Type: domain.EventTaskRetried, Payload: map[string]any{
		"triggered_by": operator, "remote_addr": remoteAddr, "retry_count": rec.State.RetryCount + 1,
	}}
	state, err := r.deps.Store.RetryTask(ctx, taskID, r.deps.MaxRetries, ev)
	if err != nil {
		if errors.Is(err, domain.ErrNotRetryable) {
			return nil, ErrNotRetryable
		}
		return nil, fmt.Errorf("taskrunner: retry %s: %w", taskID, err)
	}
	slog.InfoContext(ctx, "task retried by operator", "operator", operator, "remote_addr", remoteAddr, "retry_count", state.RetryCount)

	done := make(chan Result, 1)
	go func() {
		// Deliberately not ctx: the continuation outlives the admin request.
		bg := context.Background()
		round := state.CurrentRound + 1
		alreadyCompleted, err := r.loadAlreadyCompleted(bg, rec.Task, round)
		if err != nil {
			if !r.failUnreconcilable(bg, rec.Task, round, err, done) {
				r.fail(bg, rec.Task, round, err, done)
			}
			close(done)
			return
		}
		r.runLoop(bg, rec.Task, round, state.PriorFindings, alreadyCompleted, domain.RoundResumeRetry, done)
	}()
	return done, nil
}

// Retry is StartRetry that waits for the continuation's terminal Result.
func (r *Runner) Retry(ctx context.Context, taskID, operator, remoteAddr string) (Result, error) {
	done, err := r.StartRetry(ctx, taskID, operator, remoteAddr)
	if err != nil {
		return Result{}, err
	}
	return <-done, nil
}
