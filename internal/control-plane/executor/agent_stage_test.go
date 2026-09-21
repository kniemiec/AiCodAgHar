package executor_test

import (
	"context"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
)

func TestAgentStageExecutor_ParsesBlockingFindingFromStructuredResult(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		},
	}
	auditWriter := &fakes.FakeAuditWriter{}
	e := &executor.AgentStageExecutor{Launcher: launcher, Secrets: &fakes.FakeSecretsClient{}, Audit: auditWriter, ForgejoURL: "http://forgejo:3000"}

	run, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo", Branch: "feature", PRNumber: 7},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: true, BotAccount: "review-bot"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !run.HasFinding {
		t.Fatal("expected HasFinding=true from structured result has_blocking_finding=true")
	}
	if !run.Blocking {
		t.Fatal("expected StepRun.Blocking to carry through from the resolved step")
	}

	launches := launcher.Launches()
	if len(launches) != 1 || launches[0].StepType != executor.StepTypeAgentStage {
		t.Fatalf("expected one agent-stage Launch call, got %+v", launches)
	}
	if launches[0].PayloadRef != "review" {
		t.Fatalf("expected payload_ref to be the step ID, got %q", launches[0].PayloadRef)
	}
	env := launches[0].Env
	if env["TARGET_REPO"] != "owner/repo" || env["BRANCH"] != "feature" || env["PR_NUMBER"] != "7" || env["FORGEJO_URL"] != "http://forgejo:3000" {
		t.Fatalf("unexpected launch env: %+v", env)
	}
	if env["FORGEJO_TOKEN"] == "" {
		t.Fatal("expected a minted credential token in the launch env")
	}

	chain, err := auditWriter.GetTaskChain(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected one audited invocation, got %d", len(chain))
	}
	if chain[0].StepID != "review" || chain[0].StepType != executor.StepTypeAgentStage || chain[0].Status != "completed" {
		t.Fatalf("unexpected audit record: %+v", chain[0])
	}
}

// TestAgentStageExecutor_PropagatesTaskDescription exercises Task Intent
// Propagation design §6: a non-empty Task.Description reaches the launch
// env as TASK_DESCRIPTION, and is omitted entirely (not sent empty) when
// the task has none.
func TestAgentStageExecutor_PropagatesTaskDescription(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	e := &executor.AgentStageExecutor{Launcher: launcher, Secrets: &fakes.FakeSecretsClient{}, Audit: &fakes.FakeAuditWriter{}}

	_, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo", Description: "implement X"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "implement", Type: executor.StepTypeAgentStage, BotAccount: "coding-agent"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := launcher.Launches()[0].Env["TASK_DESCRIPTION"]; got != "implement X" {
		t.Fatalf("expected TASK_DESCRIPTION=%q, got %q", "implement X", got)
	}

	launcher2 := &fakes.FakeContainerLauncher{}
	e2 := &executor.AgentStageExecutor{Launcher: launcher2, Secrets: &fakes.FakeSecretsClient{}, Audit: &fakes.FakeAuditWriter{}}
	if _, err := e2.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-2", TargetRepo: "owner/repo"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "implement", Type: executor.StepTypeAgentStage, BotAccount: "coding-agent"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, ok := launcher2.Launches()[0].Env["TASK_DESCRIPTION"]; ok {
		t.Fatal("expected no TASK_DESCRIPTION key when Task.Description is empty")
	}
}

func TestAgentStageExecutor_NoFindingWhenStructuredResultAbsent(t *testing.T) {
	e := &executor.AgentStageExecutor{Launcher: &fakes.FakeContainerLauncher{}, Secrets: &fakes.FakeSecretsClient{}, Audit: &fakes.FakeAuditWriter{}}

	run, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: true, BotAccount: "review-bot"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if run.HasFinding {
		t.Fatal("expected HasFinding=false when no structured result is present")
	}
}

func TestAgentStageExecutor_RevokesCredentialAfterLaunch(t *testing.T) {
	secrets := &fakes.FakeSecretsClient{}
	e := &executor.AgentStageExecutor{Launcher: &fakes.FakeContainerLauncher{}, Secrets: secrets, Audit: &fakes.FakeAuditWriter{}}

	run, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo"},
		Round: 1,
		Step:  policyclient.ResolvedStep{StepID: "review", Type: executor.StepTypeAgentStage, BotAccount: "review-bot"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = run

	// FakeSecretsClient mints credential IDs as "cred-1", "cred-2", ... — the
	// first (and only) credential minted by this call must have been revoked.
	if !secrets.IsRevoked("cred-1") {
		t.Fatal("expected the minted credential to be revoked after Execute returns")
	}
}
