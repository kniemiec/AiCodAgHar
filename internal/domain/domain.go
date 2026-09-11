// Package domain holds the core entities the orchestrator's state machine
// (internal/coreloop) operates on: Task, Round, StepRun, and Invocation.
package domain

// Task is one unit of orchestrator work — the thing a Task-type PDP request
// and a coreloop.Run call are both scoped to.
//
// Branch, BaseBranch, and PRNumber are Phase 4 additions: the control-plane
// resolves (or creates) the review PR once, before coreloop.Run starts, and
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
type StepRun struct {
	Round            int
	StepID           string
	StepType         string // agent-stage | deterministic-step
	InvocationID     string
	Blocking         bool
	ExitCode         int32
	HasFinding       bool
	StructuredResult []byte
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
