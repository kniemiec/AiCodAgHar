package coreloop_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/approval"
	"github.com/kniemiec/AiCodAgHar/internal/audit"
	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
	"github.com/kniemiec/AiCodAgHar/internal/fakes"
)

func newRegistry(launcher launcherclient.ContainerLauncher) *executor.Registry {
	secrets := &fakes.FakeSecretsClient{}
	audit := &fakes.FakeAuditWriter{}
	return executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: audit},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: audit},
	)
}

func TestRun_PolicyDenyShortCircuitsBeforeLaunch(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{Decision: "deny", Reason: "quota exceeded"}, nil
		},
	}

	res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
	}, domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Outcome != coreloop.OutcomeStopEscalate {
		t.Fatalf("expected stop-escalate on policy deny, got %v", res.Outcome)
	}
	if res.RoundsRun != 1 {
		t.Fatalf("expected the deny to short-circuit at round 1, got RoundsRun=%d", res.RoundsRun)
	}
	if len(launcher.Launches()) != 0 {
		t.Fatalf("expected zero Launch calls when policy denies, got %d", len(launcher.Launches()))
	}
}

func TestRun_RoundCapFiresStopEscalateAtRound5(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: true},
				},
			}, nil
		},
	}

	res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
	}, domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.RoundsRun != coreloop.MaxRounds {
		t.Fatalf("expected RoundsRun=%d, got %d", coreloop.MaxRounds, res.RoundsRun)
	}
	if res.Outcome != coreloop.OutcomeStopEscalate {
		t.Fatalf("expected stop-escalate when a blocking finding persists through the round cap, got %v", res.Outcome)
	}
	if len(launcher.Launches()) != coreloop.MaxRounds {
		t.Fatalf("expected exactly %d Launch calls (one per round), got %d", coreloop.MaxRounds, len(launcher.Launches()))
	}
}

// TestRun_FanOutRunsStepsConcurrently proves fan-out is concurrent, not
// sequential: each Launch call blocks until both steps' Launch calls have
// started. If fan-out ran steps one at a time, the first call would block
// forever waiting for the second to start, and this test would time out.
func TestRun_FanOutRunsStepsConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)

	launcher := &fakes.FakeContainerLauncher{
		LaunchFunc: func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
			wg.Done()
			wg.Wait()
			return launcherclient.RunResult{InvocationID: req.InvocationID, ExitCode: 0}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: false},
					{StepID: "unit-tests", Type: executor.StepTypeDeterministicStep, Blocking: false},
				},
			}, nil
		},
	}

	done := make(chan coreloop.Result, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
		}, domain.Task{ID: "task-1"})
		if err != nil {
			errCh <- err
			return
		}
		done <- res
	}()

	select {
	case res := <-done:
		if res.Outcome != coreloop.OutcomeStopSuccess {
			t.Fatalf("expected stop-success (no blocking steps), got %v (%s)", res.Outcome, res.Reason)
		}
	case err := <-errCh:
		t.Fatalf("Run returned error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out: fan-out did not run both steps concurrently (both Launch calls must start before either returns)")
	}
}

// TestRun_AggregateEvaluateBlockingVsAdvisoryTransition exercises the
// aggregate/evaluate continue transition: a blocking finding in round 1
// keeps the loop going; once the same step becomes advisory-only (round 2),
// evaluate reaches stop-success even though the finding is still present.
func TestRun_AggregateEvaluateBlockingVsAdvisoryTransition(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			round, _ := req.Facts["round"].(int)
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: round == 1},
				},
			}, nil
		},
	}

	res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
	}, domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.RoundsRun != 2 {
		t.Fatalf("expected the loop to continue past round 1 (blocking finding) and stop at round 2 (advisory only), got RoundsRun=%d", res.RoundsRun)
	}
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success once the finding becomes advisory-only, got %v (%s)", res.Outcome, res.Reason)
	}
}

func TestRun_RequireHumanApproval(t *testing.T) {
	t.Run("not approved stops escalate before launch", func(t *testing.T) {
		launcher := &fakes.FakeContainerLauncher{}
		policy := &fakes.FakePolicyClient{
			EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
				return policyclient.PolicyDecision{Decision: "require_human_approval"}, nil
			},
		}

		res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
		}, domain.Task{ID: "task-1"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Outcome != coreloop.OutcomeStopEscalate {
			t.Fatalf("expected stop-escalate, got %v", res.Outcome)
		}
		if len(launcher.Launches()) != 0 {
			t.Fatalf("expected no launches before approval, got %d", len(launcher.Launches()))
		}
	})

	t.Run("approved proceeds to fan-out", func(t *testing.T) {
		launcher := &fakes.FakeContainerLauncher{}
		policy := &fakes.FakePolicyClient{
			EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
				return policyclient.PolicyDecision{
					Decision:      "require_human_approval",
					ResolvedSteps: []policyclient.ResolvedStep{{StepID: "review", Type: executor.StepTypeAgentStage}},
				}, nil
			},
		}

		res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
			Policy:    policy,
			Approval:  &fakes.FakeApprovalVerifier{Result: approval.ApprovalResult{Approved: true, ApprovedBy: "human"}},
			Executors: newRegistry(launcher),
			Audit:     &fakes.FakeAuditWriter{},
		}, domain.Task{ID: "task-1"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Outcome != coreloop.OutcomeStopSuccess {
			t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
		}
		if len(launcher.Launches()) != 1 {
			t.Fatalf("expected one launch after approval, got %d", len(launcher.Launches()))
		}
	})
}

// TestRun_AuditsEveryDecisionPointWithAValidChain exercises Phase 5's audit
// wiring: one "pdp-decision" row per round, one row per launched step, and
// one "task-outcome" row, all correctly hash-chained. It also pins
// resolved_steps_json's wire shape to snake_case (a real regression this
// test would have caught: policyclient.ResolvedStep initially had no json
// tags, so it marshaled as capitalized Go field names instead).
func TestRun_AuditsEveryDecisionPointWithAValidChain(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	secrets := &fakes.FakeSecretsClient{}
	auditWriter := &fakes.FakeAuditWriter{}
	// Built manually (not via newRegistry) so the executors share this same
	// auditWriter instance with coreloop.Dependencies.Audit below —
	// newRegistry wires its own separate FakeAuditWriter, which would defeat
	// the point of this test.
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
	)
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review", Type: executor.StepTypeAgentStage, BotAccount: "review-bot"},
				},
			}, nil
		},
	}

	res, err := coreloop.Run(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: registry, Audit: auditWriter,
	}, domain.Task{ID: "task-audit-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
	}

	chain, err := auditWriter.GetTaskChain(context.Background(), "task-audit-1")
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected 3 audit rows (pdp-decision, step, task-outcome), got %d: %+v", len(chain), chain)
	}
	if chain[0].StepType != "pdp-decision" || chain[1].StepType != executor.StepTypeAgentStage || chain[2].StepType != "task-outcome" {
		t.Fatalf("unexpected row ordering/types: %+v", chain)
	}
	if err := audit.VerifyChain(chain); err != nil {
		t.Fatalf("expected an intact hash chain, got: %v", err)
	}

	resolvedStepsJSON := string(chain[0].ResolvedStepsJSON)
	if !strings.Contains(resolvedStepsJSON, `"step_id"`) || strings.Contains(resolvedStepsJSON, `"StepID"`) {
		t.Fatalf("expected resolved_steps_json to use snake_case (matching api/openapi/pdp-authzen.yaml), got: %s", resolvedStepsJSON)
	}
}
