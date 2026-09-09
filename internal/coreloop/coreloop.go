// Package coreloop implements ADR-7's fixed state machine: generate (PDP
// evaluate + resolve steps) -> fan out (dispatch steps concurrently) ->
// aggregate (combine step outcomes) -> evaluate (continue / stop-success /
// stop-escalate), with a hardcoded round cap of 5.
package coreloop

import (
	"context"
	"fmt"
	"sync"

	"github.com/kniemiec/AiCodAgHar/internal/approval"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
)

// MaxRounds is ADR-7's hardcoded round cap.
const MaxRounds = 5

// Outcome is the terminal state Run reaches for a task.
type Outcome string

const (
	OutcomeStopSuccess  Outcome = "stop-success"
	OutcomeStopEscalate Outcome = "stop-escalate"
)

// Dependencies are the test-seam interfaces Run needs, plus the
// orchestrator process identity PolicyRequest carries. SecretsClient is
// wired into internal/executor directly (credential issuance happens per
// step, not per round); AuditWriter is wired starting Phase 5.
type Dependencies struct {
	Policy                 policyclient.PolicyClient
	Approval               approval.ApprovalVerifier
	Executors              *executor.Registry
	OrchestratorInstanceID string
}

// Result is Run's outcome for one task.
type Result struct {
	TaskID    string
	Outcome   Outcome
	RoundsRun int
	Reason    string
	StepRuns  []domain.StepRun
}

// Run executes the core loop for one task until it reaches a terminal
// outcome or exhausts MaxRounds.
func Run(ctx context.Context, deps Dependencies, task domain.Task) (Result, error) {
	var allRuns []domain.StepRun

	for round := 1; round <= MaxRounds; round++ {
		decision, err := deps.Policy.Evaluate(ctx, policyRequest(deps, task, round))
		if err != nil {
			return Result{}, fmt.Errorf("round %d: policy evaluate: %w", round, err)
		}

		switch decision.Decision {
		case "deny":
			return Result{
				TaskID: task.ID, Outcome: OutcomeStopEscalate, RoundsRun: round,
				Reason: "policy denied: " + decision.Reason, StepRuns: allRuns,
			}, nil

		case "require_human_approval":
			res, err := deps.Approval.VerifyApproval(ctx, approval.ApprovalCheckRequest{TargetRepo: task.TargetRepo})
			if err != nil {
				return Result{}, fmt.Errorf("round %d: verify approval: %w", round, err)
			}
			if !res.Approved {
				return Result{
					TaskID: task.ID, Outcome: OutcomeStopEscalate, RoundsRun: round,
					Reason: "human approval required and not granted", StepRuns: allRuns,
				}, nil
			}

		case "allow":
			// proceed to fan-out below

		default:
			return Result{}, fmt.Errorf("round %d: unknown policy decision %q", round, decision.Decision)
		}

		runs, err := fanOut(ctx, deps.Executors, task, round, decision.ResolvedSteps, decision.GrantedScopes)
		if err != nil {
			return Result{}, fmt.Errorf("round %d: fan out: %w", round, err)
		}
		allRuns = append(allRuns, runs...)

		if !aggregate(runs) {
			return Result{TaskID: task.ID, Outcome: OutcomeStopSuccess, RoundsRun: round, StepRuns: allRuns}, nil
		}
	}

	return Result{
		TaskID: task.ID, Outcome: OutcomeStopEscalate, RoundsRun: MaxRounds,
		Reason: "round cap reached with blocking findings outstanding", StepRuns: allRuns,
	}, nil
}

func policyRequest(deps Dependencies, task domain.Task, round int) policyclient.PolicyRequest {
	return policyclient.PolicyRequest{
		InvocationID:           fmt.Sprintf("%s:round-%d:pdp-eval", task.ID, round),
		TaskID:                 task.ID,
		TriggeringPrincipal:    task.TriggeringPrincipal,
		OrchestratorInstanceID: deps.OrchestratorInstanceID,
		BotAccount:             task.BotAccount,
		TargetRepo:             task.TargetRepo,
		TaskType:               task.Type,
		RiskTier:               task.RiskTier,
		Facts:                  map[string]any{"round": round},
	}
}

// fanOut dispatches every PDP-resolved step concurrently through the
// registry and waits for all of them to finish.
func fanOut(ctx context.Context, reg *executor.Registry, task domain.Task, round int, steps []policyclient.ResolvedStep, grantedScopes []string) ([]domain.StepRun, error) {
	runs := make([]domain.StepRun, len(steps))
	errs := make([]error, len(steps))

	var wg sync.WaitGroup
	for i, step := range steps {
		wg.Add(1)
		go func(i int, step policyclient.ResolvedStep) {
			defer wg.Done()
			exec, ok := reg.Lookup(step.Type)
			if !ok {
				errs[i] = fmt.Errorf("no executor registered for step type %q", step.Type)
				return
			}
			run, err := exec.Execute(ctx, executor.StepContext{
				Task: task, Round: round, Step: step, GrantedScopes: grantedScopes,
			})
			if err != nil {
				errs[i] = err
				return
			}
			runs[i] = run
		}(i, step)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return runs, nil
}

// aggregate combines step outcomes into a single blocking/non-blocking
// signal for the evaluate transition: a round has outstanding blocking
// findings iff any Blocking step reported one. Non-blocking (advisory)
// findings never gate continuation.
func aggregate(runs []domain.StepRun) bool {
	for _, r := range runs {
		if r.Blocking && r.HasFinding {
			return true
		}
	}
	return false
}
