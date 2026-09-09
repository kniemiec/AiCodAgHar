package executor_test

import (
	"context"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
	"github.com/kniemiec/AiCodAgHar/internal/fakes"
)

func TestDeterministicStepExecutor_NonZeroExitIsAFinding(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		LaunchFunc: func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
			return launcherclient.RunResult{InvocationID: req.InvocationID, ExitCode: 1, Status: "completed"}, nil
		},
	}
	e := &executor.DeterministicStepExecutor{Launcher: launcher, Secrets: &fakes.FakeSecretsClient{}}

	run, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "unit-tests", Type: executor.StepTypeDeterministicStep, Blocking: true, BotAccount: "ci-bot"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !run.HasFinding {
		t.Fatal("expected non-zero exit code to be treated as a finding")
	}
	if launcher.Launches()[0].PayloadRef != "unit-tests" {
		t.Fatalf("expected payload_ref to be the step ID, got %q", launcher.Launches()[0].PayloadRef)
	}
}

func TestDeterministicStepExecutor_ZeroExitIsNotAFinding(t *testing.T) {
	e := &executor.DeterministicStepExecutor{Launcher: &fakes.FakeContainerLauncher{}, Secrets: &fakes.FakeSecretsClient{}}

	run, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "unit-tests", Type: executor.StepTypeDeterministicStep, Blocking: true, BotAccount: "ci-bot"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if run.HasFinding {
		t.Fatal("expected exit code 0 (the fake's default) to not be a finding")
	}
}
