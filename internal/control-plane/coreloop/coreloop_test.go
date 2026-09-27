package coreloop_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
)

func newRegistry(launcher launcherclient.ContainerLauncher) *executor.Registry {
	secrets := &fakes.FakeSecretsClient{}
	audit := &fakes.FakeAuditWriter{}
	return executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: audit},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: audit},
	)
}

func TestRunRound_PolicyDenyReturnsStopEscalate(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{Decision: "deny", Reason: "quota exceeded"}, nil
		},
	}

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
	}, domain.Task{ID: "task-1"}, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}

	if res.Outcome != coreloop.OutcomeStopEscalate {
		t.Fatalf("expected stop-escalate on policy deny, got %v", res.Outcome)
	}
	if len(launcher.Launches()) != 0 {
		t.Fatalf("expected zero Launch calls when policy denies, got %d", len(launcher.Launches()))
	}
}

// TestRunRound_FanOutRunsStepsConcurrently proves fan-out is concurrent,
// not sequential: each Launch call blocks until both steps' Launch calls
// have started. If fan-out ran steps one at a time, the first call would
// block forever waiting for the second to start, and this test would time
// out.
func TestRunRound_FanOutRunsStepsConcurrently(t *testing.T) {
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

	done := make(chan coreloop.RoundResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
		}, domain.Task{ID: "task-1"}, 1, nil, nil)
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
		t.Fatalf("RunRound returned error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out: fan-out did not run both steps concurrently (both Launch calls must start before either returns)")
	}
}

// TestRunRound_ApprovePhase exercises design §4.4's approve gate: an unmet
// gate opens (or reuses) the approval issue and pauses without ever
// dispatching publish; an already-verified approval skips straight through
// to publish and stop-success.
func TestRunRound_ApprovePhase(t *testing.T) {
	t.Run("unapproved opens the issue and waits, without dispatching publish", func(t *testing.T) {
		launcher := &fakes.FakeContainerLauncher{}
		policy := &fakes.FakePolicyClient{
			EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
				return policyclient.PolicyDecision{
					Decision: "allow",
					ResolvedSteps: []policyclient.ResolvedStep{
						{StepID: "human-signoff", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseApprove},
						{StepID: "notify", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhasePublish},
					},
				}, nil
			},
		}
		git := &fakes.FakeGitHostClient{}

		res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
			Audit: &fakes.FakeAuditWriter{}, IssueOpener: git,
		}, domain.Task{ID: "task-1", TargetRepo: "acme/widgets"}, 1, nil, nil)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if res.Outcome != coreloop.OutcomeWaitingApproval {
			t.Fatalf("expected waiting-approval, got %v (%s)", res.Outcome, res.Reason)
		}
		if res.ApprovalIssueNumber == 0 {
			t.Fatal("expected a non-zero ApprovalIssueNumber")
		}
		if len(launcher.Launches()) != 0 {
			t.Fatalf("expected zero launches (publish must not dispatch until approved), got %d", len(launcher.Launches()))
		}
	})

	t.Run("already approved proceeds to publish and stop-success", func(t *testing.T) {
		launcher := &fakes.FakeContainerLauncher{}
		policy := &fakes.FakePolicyClient{
			EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
				return policyclient.PolicyDecision{
					Decision: "allow",
					ResolvedSteps: []policyclient.ResolvedStep{
						{StepID: "human-signoff", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseApprove},
						{StepID: "notify", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhasePublish},
					},
				}, nil
			},
		}

		res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
			Policy:    policy,
			Approval:  &fakes.FakeApprovalVerifier{Result: approval.ApprovalResult{Approved: true, ApprovedBy: "human"}},
			Executors: newRegistry(launcher),
			Audit:     &fakes.FakeAuditWriter{},
		}, domain.Task{ID: "task-1"}, 1, nil, nil)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if res.Outcome != coreloop.OutcomeStopSuccess {
			t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
		}
		if len(launcher.Launches()) != 1 || launcher.Launches()[0].StepID != "notify" {
			t.Fatalf("expected exactly one launch, for the publish-phase step, got %+v", launcher.Launches())
		}
	})
}

// TestRunRound_AuditsPDPDecisionAndStepRuns exercises Phase 5's audit
// wiring within a single round: one "pdp-decision" row and one row per
// launched step, correctly hash-chained. The task-outcome row is now
// internal/control-plane/taskrunner's responsibility (it's the only thing that knows a
// round's outcome is the *task's* terminal outcome), so it's not part of
// this round-scoped chain — see taskrunner_test.go for that. This test
// also pins resolved_steps_json's wire shape to snake_case (a real
// regression this test would have caught: policyclient.ResolvedStep
// initially had no json tags, so it marshaled as capitalized Go field
// names instead).
func TestRunRound_AuditsPDPDecisionAndStepRuns(t *testing.T) {
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

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: registry, Audit: auditWriter,
	}, domain.Task{ID: "task-audit-1"}, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
	}

	chain, err := auditWriter.GetTaskChain(context.Background(), "task-audit-1")
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 audit rows (pdp-decision, step), got %d: %+v", len(chain), chain)
	}
	if chain[0].StepType != "pdp-decision" || chain[1].StepType != executor.StepTypeAgentStage {
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

// TestRunRound_GeneratePhaseDispatchedBeforeReviewPhase exercises §4.1's
// phase dispatch order: the generate-phase step launches alone, before
// either review-phase step, regardless of the order PDP resolved them in.
func TestRunRound_GeneratePhaseDispatchedBeforeReviewPhase(t *testing.T) {
	var mu sync.Mutex
	var order []string

	launcher := &fakes.FakeContainerLauncher{
		LaunchFunc: func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
			mu.Lock()
			order = append(order, req.StepID)
			mu.Unlock()
			return launcherclient.RunResult{InvocationID: req.InvocationID, ExitCode: 0}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review-a", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview},
					{StepID: "implement", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate},
					{StepID: "review-b", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseReview},
				},
			}, nil
		},
	}

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
		PRCreator: &fakes.FakeGitHostClient{},
	}, domain.Task{ID: "task-1"}, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 launches, got %d: %v", len(order), order)
	}
	if order[0] != "implement" {
		t.Fatalf("expected the generate-phase step to launch first regardless of ResolvedSteps order, got launch order %v", order)
	}
	remaining := map[string]bool{order[1]: true, order[2]: true}
	if !remaining["review-a"] || !remaining["review-b"] {
		t.Fatalf("expected both review-phase steps to launch after generate, got launch order %v", order)
	}
}

// TestRunRound_CreatesBranchBeforeOpeningPR exercises the
// orchestrator-owned-branch-creation design: once the generate phase
// completes without a blocking failure, RunRound creates (or reuses) the
// task's branch before opening its review PR — closing the case where the
// generate phase's own step completed without pushing any commits, which
// would otherwise leave CreateOrUpdatePR targeting a branch that never
// came to exist.
func TestRunRound_CreatesBranchBeforeOpeningPR(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "implement", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate}},
			}, nil
		},
	}
	git := &fakes.FakeGitHostClient{}
	task := domain.Task{ID: "task-1", TargetRepo: "acme/widgets", Branch: "feature", BaseBranch: "main"}

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
		PRCreator: git,
	}, task, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
	}
	if res.PRNumber == 0 {
		t.Fatal("expected a non-zero PRNumber")
	}

	// If RunRound already created the branch, reusing it now must return
	// the same branch rather than erroring or creating a second one.
	reused, err := git.CreateOrReuseBranch(context.Background(), gitclient.RepoRef{Owner: "acme", Name: "widgets"}, "feature", "main")
	if err != nil {
		t.Fatalf("CreateOrReuseBranch: %v", err)
	}
	if reused.Name != "feature" {
		t.Fatalf("expected RunRound to have already created branch %q, got %+v", "feature", reused)
	}
}

// TestRunRound_CardinalityViolation_TwoGenerateStepsFailsClosed exercises
// §4.1's cardinality rule: a second generate-phase step in one round is a
// fail-closed config error, not a race between the two.
func TestRunRound_CardinalityViolation_TwoGenerateStepsFailsClosed(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "implement-a", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate},
					{StepID: "implement-b", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate},
				},
			}, nil
		},
	}

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
	}, domain.Task{ID: "task-1"}, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != coreloop.OutcomeStopEscalate {
		t.Fatalf("expected stop-escalate for a second generate-phase step, got %v", res.Outcome)
	}
	if !strings.Contains(res.Reason, "generate") {
		t.Fatalf("expected reason to mention the generate-phase cardinality violation, got %q", res.Reason)
	}
	if len(launcher.Launches()) != 0 {
		t.Fatalf("expected zero launches on a cardinality violation, got %d", len(launcher.Launches()))
	}
}

// TestRunRound_ApprovePhaseNeverDispatchedThroughStepExecutor pins §4.4's
// invariant: an approve-phase resolved step is never dispatched through
// StepExecutor (no container to launch for "wait until a human says yes") —
// even if it's the only step present, a FakeContainerLauncher that would
// fail any Launch call proves nothing was ever attempted.
func TestRunRound_ApprovePhaseNeverDispatchedThroughStepExecutor(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		LaunchFunc: func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
			t.Fatalf("unexpected Launch call for approve-phase step %s", req.StepID)
			return launcherclient.RunResult{}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "human-signoff", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseApprove},
				},
			}, nil
		},
	}

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher),
		Audit: &fakes.FakeAuditWriter{}, IssueOpener: &fakes.FakeGitHostClient{},
	}, domain.Task{ID: "task-1"}, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != coreloop.OutcomeWaitingApproval {
		t.Fatalf("expected waiting-approval, got %v (%s)", res.Outcome, res.Reason)
	}
}

// TestRunRound_PriorFindingsPlumbedAcrossManualRounds exercises §4.1's
// PRIOR_FINDINGS_JSON plumbing directly against RunRound's own contract —
// no taskrunner involved: round 1's blocking review finding, returned as
// RoundResult.PriorFindings, is fed by hand into round 2's call, and must
// reach round 2's generate-phase step (and only that step) as
// PRIOR_FINDINGS_JSON; round 1's generate step, with nothing prior, gets
// none at all.
func TestRunRound_PriorFindingsPlumbedAcrossManualRounds(t *testing.T) {
	var mu sync.Mutex
	envByInvocation := map[string]map[string]string{}

	launcher := &fakes.FakeContainerLauncher{
		LaunchFunc: func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
			mu.Lock()
			envByInvocation[req.InvocationID] = req.Env
			mu.Unlock()
			return launcherclient.RunResult{InvocationID: req.InvocationID, ExitCode: 0}, nil
		},
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			if strings.Contains(invocationID, ":1:review") {
				return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
			}
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": false}`)}, nil
		},
	}
	stepsForRound := func(round int) []policyclient.ResolvedStep {
		return []policyclient.ResolvedStep{
			{StepID: "implement", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate, Blocking: false},
			{StepID: "review", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: round == 1},
		}
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			round, _ := req.Facts["round"].(int)
			return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: stepsForRound(round)}, nil
		},
	}
	deps := coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
		PRCreator: &fakes.FakeGitHostClient{},
	}
	task := domain.Task{ID: "task-1"}

	round1, err := coreloop.RunRound(context.Background(), deps, task, 1, nil, nil)
	if err != nil {
		t.Fatalf("RunRound round 1: %v", err)
	}
	if round1.Outcome != coreloop.OutcomeContinue {
		t.Fatalf("expected round 1 to continue (blocking review finding), got %v", round1.Outcome)
	}

	round2, err := coreloop.RunRound(context.Background(), deps, task, 2, round1.PriorFindings, nil)
	if err != nil {
		t.Fatalf("RunRound round 2: %v", err)
	}
	if round2.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected round 2 to stop-success (advisory only), got %v (%s)", round2.Outcome, round2.Reason)
	}

	mu.Lock()
	defer mu.Unlock()

	round1Env, ok := envByInvocation["task-1:1:implement"]
	if !ok {
		t.Fatal("expected a round-1 implement launch")
	}
	if _, ok := round1Env["PRIOR_FINDINGS_JSON"]; ok {
		t.Fatalf("expected no PRIOR_FINDINGS_JSON on round 1 (nothing prior), got %q", round1Env["PRIOR_FINDINGS_JSON"])
	}

	round2Env, ok := envByInvocation["task-1:2:implement"]
	if !ok {
		t.Fatal("expected a round-2 implement launch")
	}
	pf := round2Env["PRIOR_FINDINGS_JSON"]
	if pf == "" {
		t.Fatal("expected round 2's implement step to receive PRIOR_FINDINGS_JSON from round 1's blocking review finding")
	}
	if !strings.Contains(pf, `"step_id":"review"`) {
		t.Fatalf("expected PRIOR_FINDINGS_JSON to reference the review step, got %s", pf)
	}

	round2ReviewEnv := envByInvocation["task-1:2:review"]
	if _, ok := round2ReviewEnv["PRIOR_FINDINGS_JSON"]; ok {
		t.Fatal("expected PRIOR_FINDINGS_JSON to be scoped to the generate-phase step only, not review")
	}
}

// TestRunRound_AlreadyCompletedStepsAreSkipped exercises the
// alreadyCompleted parameter internal/control-plane/taskrunner's startup reconciliation
// relies on (design §4.2): a step present in alreadyCompleted is not
// re-dispatched — its cached result is reused as-is — while every other
// step in the same round dispatches normally.
func TestRunRound_AlreadyCompletedStepsAreSkipped(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "review-a", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: true},
					{StepID: "review-b", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseReview, Blocking: true},
				},
			}, nil
		},
	}
	cached := domain.StepRun{
		StepID: "review-a", StepType: executor.StepTypeAgentStage, InvocationID: "task-1:1:review-a",
		Blocking: true, HasFinding: false, Status: domain.StepRunStatusCompleted,
	}

	res, err := coreloop.RunRound(context.Background(), coreloop.Dependencies{
		Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Executors: newRegistry(launcher), Audit: &fakes.FakeAuditWriter{},
	}, domain.Task{ID: "task-1"}, 1, nil, map[string]domain.StepRun{"review-a": cached})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}

	if len(launcher.Launches()) != 1 || launcher.Launches()[0].StepID != "review-b" {
		t.Fatalf("expected exactly one launch, for review-b only (review-a is cached), got %+v", launcher.Launches())
	}
	if len(res.StepRuns) != 2 {
		t.Fatalf("expected both steps represented in StepRuns (one dispatched, one cached), got %d: %+v", len(res.StepRuns), res.StepRuns)
	}
	// review-b defaults to a non-blocking, non-finding outcome (FakeContainerLauncher's
	// zero-value ExtractLogs), and the cached review-a has HasFinding: false — so
	// this round should reach stop-success, not loop.
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success, got %v (%s)", res.Outcome, res.Reason)
	}
}
