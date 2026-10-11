package domain

import (
	"errors"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
)

// ErrNotRetryable is returned by TaskStore.RetryTask (and Runner.Retry) when
// the task is not a retryable failed task with retries left.
var ErrNotRetryable = errors.New("task not retryable")

// TaskEvent is one row of orchestrator.task_events: an append-only fact about
// a task's lifecycle (platform-observability design §3). ID and At are
// assigned by the store on insert and are only meaningful on read.
type TaskEvent struct {
	ID           int64
	At           time.Time
	TaskID       string
	Round        int
	StepID       string
	InvocationID string
	Type         string
	Payload      map[string]any
	TraceID      string
	SpanID       string
}

// EventFilter narrows ListEvents (GET /tasks/{id}/events).
type EventFilter struct {
	Type    string
	Since   time.Time
	Until   time.Time
	AfterID int64
	Limit   int // 0 = unlimited (internal callers); the HTTP layer applies its own cap
}

// FailureRecord is what Runner.fail hands TaskStore.FailTask.
type FailureRecord struct {
	TaskID    string
	Round     int
	Class     string
	Retryable bool
	Reason    string
}

// TaskAge is one row of the admin "tasks by status" listing.
type TaskAge struct {
	TaskID string
	Status string
	Since  time.Time
}

// Task event types (design §3 catalogue). The vocabulary is open; consumers
// must ignore types they don't know.
const (
	EventTaskAccepted      = "task.accepted"
	EventTaskMigrated      = "task.migrated"
	EventTaskReconciled    = "task.reconciled"
	EventRoundStarted      = "round.started"
	EventRoundResumed      = "round.resumed"
	EventPolicyDecided     = "policy.decided"
	EventPhaseStarted      = "phase.started"
	EventPhaseFinished     = "phase.finished"
	EventStepLaunched      = "step.launched"
	EventStepHeartbeat     = "step.heartbeat"
	EventStepResolved      = "step.resolved"
	EventStepReconciled    = "step.reconciled"
	EventApprovalRequested = "approval.requested"
	EventApprovalGranted   = "approval.granted"
	EventApprovalRejected  = "approval.rejected"
	EventRoundFinished     = "round.finished"
	EventTaskFailed        = "task.failed"
	EventTaskRetried       = "task.retried"
	EventTaskFinished      = "task.finished"
)

// ReasonKind values: why a task ended stop_escalate. These are governance
// outcomes, never failures (ER-3).
const (
	ReasonPolicyDenied       = "policy_denied"
	ReasonInvalidPhaseConfig = "invalid_phase_config"
	ReasonRoundCap           = "round_cap"
	ReasonApprovalRejected   = "approval_rejected"
)

// Event builders shared by PostgresTaskStore and the in-memory fake, so both
// derive identical events for the row changes that carry their own.

// TaskAcceptedEvent is written by CreateTask in the same transaction.
func TaskAcceptedEvent(t Task) TaskEvent {
	return TaskEvent{TaskID: t.ID, Type: EventTaskAccepted, Payload: map[string]any{
		"target_repo": t.TargetRepo, "risk_tier": t.RiskTier, "base_branch": t.BaseBranch,
	}}
}

// RoundStartedEvent is written by EnsureRound when it inserted the row.
func RoundStartedEvent(taskID string, round int) TaskEvent {
	return TaskEvent{TaskID: taskID, Round: round, Type: EventRoundStarted, Payload: map[string]any{}}
}

// StepLaunchedEvent is written by RecordStepLaunched in the same transaction.
func StepLaunchedEvent(taskID string, round int, stepID, invocationID, stepType, phase string, blocking bool) TaskEvent {
	return TaskEvent{TaskID: taskID, Round: round, StepID: stepID, InvocationID: invocationID, Type: EventStepLaunched,
		Payload: map[string]any{"step_type": stepType, "phase": phase, "blocking": blocking}}
}

// StepResolvedEvent is written by CompleteStepRun in the same transaction.
// execErr is the executor's own error (nil on success).
func StepResolvedEvent(taskID string, round int, stepID, invocationID string, run StepRun, execErr error, duration time.Duration) TaskEvent {
	payload := map[string]any{"duration_ms": duration.Milliseconds()}
	if execErr != nil {
		class, _, dep := failure.Classify(execErr)
		payload["outcome"] = StepRunStatusFailed
		payload["error_class"] = string(class)
		if dep != "" {
			payload["dependency"] = dep
		}
	} else {
		payload["outcome"] = StepRunStatusCompleted
		payload["exit_code"] = run.ExitCode
		payload["has_finding"] = run.HasFinding
	}
	return TaskEvent{TaskID: taskID, Round: round, StepID: stepID, InvocationID: invocationID, Type: EventStepResolved, Payload: payload}
}

// WaitActive computes a task's wall, wait and active durations from its
// ordered events (design §4): wait = Σ(approval.granted|rejected − preceding
// approval.requested) + Σ(task.retried − preceding task.failed); wall =
// end − task.accepted; active = wall − wait. end is the terminal instant
// (the caller's "now": the finishing event isn't written yet).
func WaitActive(events []TaskEvent, end time.Time) (wall, wait, active time.Duration) {
	var accepted, waitSince time.Time
	for _, e := range events {
		switch e.Type {
		case EventTaskAccepted:
			if accepted.IsZero() {
				accepted = e.At
			}
		case EventApprovalRequested, EventTaskFailed:
			waitSince = e.At
		case EventApprovalGranted, EventApprovalRejected, EventTaskRetried:
			if !waitSince.IsZero() {
				wait += e.At.Sub(waitSince)
				waitSince = time.Time{}
			}
		}
	}
	if accepted.IsZero() {
		return 0, wait, 0
	}
	wall = end.Sub(accepted)
	return wall, wait, wall - wait
}

// round.resumed payload reasons.
const (
	RoundResumeReconcile = "reconcile"
	RoundResumeApproval  = "approval"
	RoundResumeRetry     = "retry"
)
