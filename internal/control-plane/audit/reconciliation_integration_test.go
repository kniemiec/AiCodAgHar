//go:build integration

// Policy-gate reconciliation: launch count vs. PDP-allow count. This is an
// offline query/test, not something the running system checks itself — it
// drives a few real scenarios through coreloop.RunRound (against the real
// Postgres AuditWriter, fakes for everything else) and then asserts, via
// SQL, that step-level audit rows exist for a (task_id, round) if and only
// if that round's pdp-decision row says "allow", and that the count always
// matches the number of resolved steps.
package audit_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
)

func TestPolicyGateReconciliation(t *testing.T) {
	dsn := os.Getenv("AUDIT_DB_DSN")
	if dsn == "" {
		t.Skip("AUDIT_DB_DSN not set; source deploy/.env first")
	}
	writer, err := audit.NewPostgresWriter(dsn)
	if err != nil {
		t.Fatalf("NewPostgresWriter: %v", err)
	}
	defer writer.Close()

	runID := os.Getpid()
	allowTaskID := fmt.Sprintf("recon-allow-%d", runID)
	denyTaskID := fmt.Sprintf("recon-deny-%d", runID)
	unapprovedTaskID := fmt.Sprintf("recon-unapproved-%d", runID)

	// Scenario A: allow, two resolved steps — both should launch.
	runScenario(t, writer, allowTaskID, &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review", Type: executor.StepTypeAgentStage, BotAccount: "review-bot"},
					{StepID: "unit-tests", Type: executor.StepTypeDeterministicStep, BotAccount: "ci-bot"},
				},
			}, nil
		},
	}, &fakes.FakeApprovalVerifier{})

	// Scenario B: deny — zero launches.
	runScenario(t, writer, denyTaskID, &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{Decision: "deny", Reason: "quota exceeded"}, nil
		},
	}, &fakes.FakeApprovalVerifier{})

	// Scenario C: allow, with an unmet approve-phase gate (design §4.4) and
	// no generate/review steps at all — zero launches, since approve is
	// never dispatched through StepExecutor.
	runScenario(t, writer, unapprovedTaskID, &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "human-signoff", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseApprove}},
			}, nil
		},
	}, &fakes.FakeApprovalVerifier{})

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	t.Run("allow rounds: launch count matches resolved_steps count", func(t *testing.T) {
		var expected, actual int
		err := db.QueryRowContext(ctx, `
			SELECT jsonb_array_length(d.resolved_steps::jsonb),
			       (SELECT count(*) FROM audit.invocation_audit s
			          WHERE s.task_id = d.task_id AND s.round = d.round
			            AND s.step_type IN ('agent-stage','deterministic-step'))
			FROM audit.invocation_audit d
			WHERE d.task_id = $1 AND d.step_type = 'pdp-decision'`, allowTaskID,
		).Scan(&expected, &actual)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if expected != actual {
			t.Fatalf("policy-gate reconciliation failed: resolved_steps says %d, but %d step rows were launched", expected, actual)
		}
		if expected != 2 {
			t.Fatalf("expected 2 resolved steps in the allow scenario, got %d", expected)
		}
	})

	for _, tc := range []struct {
		name   string
		taskID string
	}{
		{"deny rounds: zero launches", denyTaskID},
		{"unmet approve-phase gate rounds: zero launches", unapprovedTaskID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var launchCount int
			err := db.QueryRowContext(ctx, `
				SELECT count(*) FROM audit.invocation_audit
				WHERE task_id = $1 AND step_type IN ('agent-stage','deterministic-step')`, tc.taskID,
			).Scan(&launchCount)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if launchCount != 0 {
				t.Fatalf("policy-gate reconciliation failed: expected 0 launches for a non-allow decision, got %d", launchCount)
			}
		})
	}
}

func runScenario(t *testing.T, writer *audit.PostgresWriter, taskID string, policy *fakes.FakePolicyClient, appr *fakes.FakeApprovalVerifier) {
	t.Helper()
	launcher := &fakes.FakeContainerLauncher{}
	secrets := &fakes.FakeSecretsClient{}
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: writer},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: writer},
	)

	_, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: appr, Audit: writer, Executors: registry, OrchestratorInstanceID: "recon-test",
		IssueOpener: &fakes.FakeGitHostClient{},
	}, domain.Task{ID: taskID, TargetRepo: "owner/repo"}, 1, nil, nil)
	if err != nil {
		t.Fatalf("coreloop.RunRound(%s): %v", taskID, err)
	}
}
