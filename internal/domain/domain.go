// Package domain holds the core entities the orchestrator's state machine
// (internal/coreloop) operates on: Task, Round, StepRun, and Invocation.
package domain

import "time"

// Task is one unit of orchestrator work — the thing a Task-type PDP request
// and internal/taskrunner's per-task loop (each round dispatched via
// coreloop.RunRound) are both scoped to.
//
// Branch, BaseBranch, and PRNumber are Phase 4 additions: the control-plane
// resolves (or creates) the review PR once, before the task starts, and
// every step executor needs that PR number and branch to build its
// LaunchRequest env (see internal/executor's credential/env helpers).
type Task struct {
	ID                  string
	Type                string // new-task | pr-review-round | review | security-triage
	TargetRepo          string
	TriggeringPrincipal string
	BotAccount          string
	RiskTier            string
	Branch              string
	BaseBranch          string
	PRNumber            int
}

// Round is one iteration of ADR-7's fixed core loop for a given Task.
// internal/coreloop hardcodes a round cap of 5.
type Round struct {
	TaskID string
	Number int
}

// StepRun is the outcome of dispatching one PDP-resolved step through the
// StepExecutor registry (internal/executor) for one Round. HasFinding is a
// Phase 2 stub signal — the aggregate/evaluate transition in internal/coreloop
// treats a StepRun as blocking iff Blocking && HasFinding.
//
// StructuredResult is a Phase 6 addition: the raw
// ExtractLogsResponse.StructuredResultJSON both executor types now surface
// on their StepRun, not just parse internally for HasFinding — the e2e
// smoke test asserts the deterministic-step executor's structured result is
// present on its step_run.
//
// Status is a design-follow-up addition (docs/designs/next/full-pipeline-
// async-approval-design.md §4.2): internal/taskrunner's persistence layer
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
}

// step_runs.status values (design §4.2/§5).
const (
	StepRunStatusLaunched  = "launched"
	StepRunStatusCompleted = "completed"
	StepRunStatusFailed    = "failed"
)

// TaskState is a Task's live, persisted orchestrator state (design §4.2) —
// kept separate from Task's immutable request-time config because only
// TaskState changes as internal/taskrunner steps a task through rounds and
// (eventually, §4.4) pauses it for approval.
type TaskState struct {
	TaskID              string
	Status              string // running | waiting_approval | stop_success | stop_escalate
	CurrentRound        int
	ApprovalIssueNumber *int
	ApprovedBy          string
	PriorFindings       []byte
	Reason              string
	UpdatedAt           time.Time
}

// orchestrator.tasks.status values (design §4.2). Note the underscores —
// this is the DB-persisted vocabulary, distinct from coreloop.Outcome's
// hyphenated "stop-success"/"stop-escalate" wire/API vocabulary;
// internal/taskrunner is responsible for translating between the two.
const (
	TaskStatusRunning         = "running"
	TaskStatusWaitingApproval = "waiting_approval"
	TaskStatusStopSuccess     = "stop_success"
	TaskStatusStopEscalate    = "stop_escalate"
)

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
