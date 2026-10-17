// Package domain holds the core entities the orchestrator's state machine
// (internal/control-plane/coreloop) operates on: Task, Round, StepRun, and Invocation.
package domain

import "time"

// Task is one unit of orchestrator work — the thing a Task-type PDP request
// and internal/control-plane/taskrunner's per-task loop (each round dispatched via
// coreloop.RunRound) are both scoped to.
//
// Branch and BaseBranch are Phase 4 additions: every step executor needs
// them to build its LaunchRequest env (see internal/control-plane/executor's
// credential/env helpers). PRNumber starts at 0 — a task no longer arrives
// with a branch/PR already open (Task Intent Propagation design §5):
// coreloop itself opens the review PR once the generate-phase step
// completes with no blocking failure, and persists the result back onto
// this field.
type Task struct {
	ID                  string
	Type                string // new-task (only live value; field kept for future extensibility)
	TargetRepo          string
	TriggeringPrincipal string
	BotAccount          string
	RiskTier            string
	Branch              string
	BaseBranch          string
	PRNumber            int
	// Description is the human-authored task description (Task Intent
	// Propagation design §1/§2) — a Forgejo issue's body (§3) or POST
	// /tasks' own description field — propagated into the implement and
	// review-stage prompts as untrusted context (§6/§7).
	Description string
}

// Round is one iteration of ADR-7's fixed core loop for a given Task.
// internal/control-plane/coreloop hardcodes a round cap of 5.
type Round struct {
	TaskID string
	Number int
}

// StepRun is the outcome of dispatching one PDP-resolved step through the
// StepExecutor registry (internal/control-plane/executor) for one Round. HasFinding is a
// Phase 2 stub signal — the aggregate/evaluate transition in internal/control-plane/coreloop
// treats a StepRun as blocking iff Blocking && HasFinding.
//
// StructuredResult is a Phase 6 addition: the raw
// ExtractLogsResponse.StructuredResultJSON both executor types now surface
// on their StepRun, not just parse internally for HasFinding — the e2e
// smoke test asserts the deterministic-step executor's structured result is
// present on its step_run.
//
// Status is a design-follow-up addition (docs/designs/next/full-pipeline-
// async-approval-design.md §4.2): internal/control-plane/taskrunner's persistence layer
// only, populated on read from orchestrator.step_runs — StepExecutor
// implementations never set it, since "launched" precedes an executor even
// returning.
type StepRun struct {
	Round            int
	StepID           string
	StepType         string // agent-stage | deterministic-step
	InvocationID     string
	Blocking         bool
	ExitCode         int32
	HasFinding       bool
	StructuredResult []byte
	Status           string // launched | completed | failed ("" when not persistence-backed)

	// Launch is what the launcher reported about this step's invocation
	// (outcome and timings, platform-observability design §7). It is set on
	// failed steps too, when the launcher got far enough to report. Not
	// persisted in step_runs; it travels into the step.resolved event.
	Launch LaunchInfo
}

// LaunchInfo is the launcher's own account of one invocation.
type LaunchInfo struct {
	Outcome          string // completed | boot_failure | timeout | guest_error | killed; "" if unknown
	QueueWait        time.Duration
	Boot             time.Duration
	GuestRun         time.Duration
	EffectiveTimeout time.Duration
}

// step_runs.status values (design §4.2/§5).
const (
	StepRunStatusLaunched  = "launched"
	StepRunStatusCompleted = "completed"
	StepRunStatusFailed    = "failed"
)

// TaskState is a Task's live, persisted orchestrator state (design §4.2) —
// kept separate from Task's immutable request-time config because only
// TaskState changes as internal/control-plane/taskrunner steps a task through rounds and
// (eventually, §4.4) pauses it for approval.
type TaskState struct {
	TaskID              string
	Status              string // running | waiting_approval | stop_success | stop_escalate | failed
	CurrentRound        int
	ApprovalIssueNumber *int
	ApprovedBy          string
	PriorFindings       []byte
	Reason              string
	UpdatedAt           time.Time

	// Failure bookkeeping (design §2): set while Status == failed, cleared by
	// a retry. FinishedAt is set on stop_success, stop_escalate and a
	// terminal failed; nil otherwise. AcceptedAt is tasks.created_at.
	ErrorClass string
	Retryable  bool
	RetryCount int
	AcceptedAt time.Time
	FinishedAt *time.Time
}

// orchestrator.tasks.status values (design §4.2). Note the underscores —
// this is the DB-persisted vocabulary, distinct from coreloop.Outcome's
// hyphenated "stop-success"/"stop-escalate" wire/API vocabulary;
// internal/control-plane/taskrunner is responsible for translating between the two.
const (
	TaskStatusRunning         = "running"
	TaskStatusWaitingApproval = "waiting_approval"
	TaskStatusStopSuccess     = "stop_success"
	TaskStatusStopEscalate    = "stop_escalate"
	TaskStatusFailed          = "failed"
)

// TaskStatuses is every task status, for gauges that report zero counts too.
var TaskStatuses = []string{TaskStatusRunning, TaskStatusWaitingApproval, TaskStatusStopSuccess, TaskStatusStopEscalate, TaskStatusFailed}

// TaskRecord is a Task joined with its live TaskState — what TaskStore.GetTask
// reads back, since both live in the same orchestrator.tasks row.
type TaskRecord struct {
	Task  Task
	State TaskState
}

// Invocation is one ContainerLauncher.Launch call, identified the same way
// across the launcher gRPC contract, the audit trail, and StepRun.
type Invocation struct {
	ID       string
	TaskID   string
	StepID   string
	Round    int
	StepType string
}
