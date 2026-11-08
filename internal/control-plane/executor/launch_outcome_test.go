package executor_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
)

// TestExecutors_LaunchErrorClass covers design §2's launch rows for both
// executors: the delivered outcome decides when present, the gRPC code
// otherwise.
func TestExecutors_LaunchErrorClass(t *testing.T) {
	withOutcome := func(outcome string) error {
		return &launcherclient.LaunchError{Outcome: outcome, Result: launcherclient.RunResult{Outcome: outcome},
			Err: status.Error(codes.Internal, "launch failed")}
	}
	cases := []struct {
		name  string
		err   error
		class failure.Class
		dep   string
	}{
		{"boot failure", withOutcome(launcherclient.OutcomeBootFailure), failure.ClassInfrastructure, ""},
		{"killed", withOutcome(launcherclient.OutcomeKilled), failure.ClassInfrastructure, ""},
		{"timeout", withOutcome(launcherclient.OutcomeTimeout), failure.ClassAgent, ""},
		{"guest error", withOutcome(launcherclient.OutcomeGuestError), failure.ClassAgent, ""},
		{"unavailable, no outcome", status.Error(codes.Unavailable, "down"), failure.ClassDependency, failure.DepLauncher},
		{"deadline, no outcome", status.Error(codes.DeadlineExceeded, "slow"), failure.ClassDependency, failure.DepLauncher},
		{"invalid argument, no outcome", status.Error(codes.InvalidArgument, "bad"), failure.ClassPlatform, ""},
		{"internal, no outcome (older launcher)", status.Error(codes.Internal, "boot"), failure.ClassInfrastructure, ""},
	}
	for _, stepType := range []string{executor.StepTypeAgentStage, executor.StepTypeDeterministicStep} {
		for _, tc := range cases {
			t.Run(stepType+"/"+tc.name, func(t *testing.T) {
				launcher := &fakes.FakeContainerLauncher{LaunchFunc: func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
					return launcherclient.RunResult{}, tc.err
				}}
				_, err := newExecutor(stepType, launcher).Execute(context.Background(), stepContext(stepType))
				class, _, dep := failure.Classify(err)
				if class != tc.class || dep != tc.dep {
					t.Fatalf("Classify = %s/%q, want %s/%q (err %v)", class, dep, tc.class, tc.dep, err)
				}
			})
		}
	}
}

// TestExecutors_CarryLaunchInfo: the launcher's outcome and timings reach
// StepRun.Launch on success and, when delivered, on a failed launch.
func TestExecutors_CarryLaunchInfo(t *testing.T) {
	result := launcherclient.RunResult{InvocationID: "inv", Status: "completed", Outcome: launcherclient.OutcomeCompleted,
		QueueWait: time.Second, Boot: 2 * time.Second, GuestRun: 3 * time.Second, EffectiveTimeout: 1800 * time.Second}
	want := domain.LaunchInfo{Outcome: launcherclient.OutcomeCompleted, QueueWait: time.Second, Boot: 2 * time.Second,
		GuestRun: 3 * time.Second, EffectiveTimeout: 1800 * time.Second}

	for _, stepType := range []string{executor.StepTypeAgentStage, executor.StepTypeDeterministicStep} {
		t.Run(stepType+"/success", func(t *testing.T) {
			launcher := &fakes.FakeContainerLauncher{LaunchFunc: func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
				return result, nil
			}}
			run, err := newExecutor(stepType, launcher).Execute(context.Background(), stepContext(stepType))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if run.Launch != want {
				t.Fatalf("Launch = %+v, want %+v", run.Launch, want)
			}
		})
		t.Run(stepType+"/failure", func(t *testing.T) {
			failed := result
			failed.Status, failed.Outcome = "timed_out", launcherclient.OutcomeTimeout
			launcher := &fakes.FakeContainerLauncher{LaunchFunc: func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
				return failed, &launcherclient.LaunchError{Result: failed, Outcome: failed.Outcome, Err: status.Error(codes.Internal, "timeout")}
			}}
			run, err := newExecutor(stepType, launcher).Execute(context.Background(), stepContext(stepType))
			if err == nil {
				t.Fatal("expected an error")
			}
			w := want
			w.Outcome = launcherclient.OutcomeTimeout
			if run.Launch != w {
				t.Fatalf("Launch = %+v, want %+v", run.Launch, w)
			}
		})
	}
}

func newExecutor(stepType string, launcher *fakes.FakeContainerLauncher) executor.StepExecutor {
	if stepType == executor.StepTypeAgentStage {
		return &executor.AgentStageExecutor{Launcher: launcher, Secrets: &fakes.FakeSecretsClient{}, Audit: &fakes.FakeAuditWriter{}}
	}
	return &executor.DeterministicStepExecutor{Launcher: launcher, Secrets: &fakes.FakeSecretsClient{}, Audit: &fakes.FakeAuditWriter{}}
}

func stepContext(stepType string) executor.StepContext {
	return executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "step", Type: stepType},
	}
}
