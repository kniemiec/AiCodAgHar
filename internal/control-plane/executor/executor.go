// Package executor defines the StepExecutor interface and the closed-set
// registry keyed by step type ("agent-stage" | "deterministic-step"), plus
// the two stub implementations, AgentStageExecutor and
// DeterministicStepExecutor. Per ADR-11, both dispatch exclusively through
// ContainerLauncher — the orchestrator's unified execution primitive.
package executor

import (
	"context"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// StepContext is everything a StepExecutor needs to run one PDP-resolved
// step for one round of one task.
type StepContext struct {
	Task          domain.Task
	Round         int
	Step          policyclient.ResolvedStep
	GrantedScopes []string

	// PriorFindings is the previous round's aggregated findings, JSON-
	// encoded — set only when dispatching a generate-phase step (design
	// docs/designs/completed/full-pipeline-async-approval-design.md §4.1). Nil
	// on round 1 and for every non-generate-phase step.
	PriorFindings []byte
}

// StepExecutor runs one resolved step and reports its outcome as a
// domain.StepRun.
type StepExecutor interface {
	Execute(ctx context.Context, sc StepContext) (domain.StepRun, error)
}
