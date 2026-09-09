package coreloop_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/approval"
	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
	"github.com/kniemiec/AiCodAgHar/internal/fakes"
)

func newRegistry(launcher launcherclient.ContainerLauncher) *executor.Registry {
	secrets := &fakes.FakeSecretsClient{}
	return executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets},
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
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
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
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
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
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
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
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
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
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
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
