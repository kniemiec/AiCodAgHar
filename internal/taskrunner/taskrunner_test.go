package taskrunner_test

import (
	"context"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/audit"
	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
	"github.com/kniemiec/AiCodAgHar/internal/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/taskrunner"
)

// newDeps wires a Runner's Dependencies against fakes, sharing one
// FakeAuditWriter between the executors and coreloop.Dependencies.Audit so
// audit-chain assertions see every row a test's task produces.
func newDeps(launcher *fakes.FakeContainerLauncher, policy *fakes.FakePolicyClient, store taskrunner.TaskStore, auditWriter *fakes.FakeAuditWriter) taskrunner.Dependencies {
	secrets := &fakes.FakeSecretsClient{}
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
	)
	return taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Audit: auditWriter, Executors: registry,
		},
		Store:    store,
		Launcher: launcher,
	}
}

func TestRunner_Start_StopSuccess(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: false}},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, &fakes.FakeAuditWriter{}))

	taskID, done, err := runner.Start(context.Background(), domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if taskID != "task-1" {
		t.Fatalf("expected taskID task-1, got %s", taskID)
	}

	result := <-done
	if result.Err != nil {
		t.Fatalf("Start result error: %v", result.Err)
	}
	if result.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success, got %v (%s)", result.Outcome, result.Reason)
	}
	if result.RoundsRun != 1 {
		t.Fatalf("expected RoundsRun=1, got %d", result.RoundsRun)
	}

	rec, ok, err := store.GetTask(context.Background(), "task-1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if rec.State.Status != domain.TaskStatusStopSuccess {
		t.Fatalf("expected persisted status stop_success, got %q", rec.State.Status)
	}
	if rec.State.CurrentRound != 1 {
		t.Fatalf("expected persisted current_round=1, got %d", rec.State.CurrentRound)
	}
}

// TestRunner_RoundCapFiresStopEscalateAtRound5 mirrors what used to be
// coreloop.Run's own round-cap test — that behavior now lives entirely in
// Runner's loop.
func TestRunner_RoundCapFiresStopEscalateAtRound5(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: true}},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, &fakes.FakeAuditWriter{}))

	_, done, err := runner.Start(context.Background(), domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-done
	if result.Err != nil {
		t.Fatalf("result error: %v", result.Err)
	}

	if result.RoundsRun != coreloop.MaxRounds {
		t.Fatalf("expected RoundsRun=%d, got %d", coreloop.MaxRounds, result.RoundsRun)
	}
	if result.Outcome != coreloop.OutcomeStopEscalate {
		t.Fatalf("expected stop-escalate when a blocking finding persists through the round cap, got %v", result.Outcome)
	}
	if len(launcher.Launches()) != coreloop.MaxRounds {
		t.Fatalf("expected exactly %d Launch calls (one per round), got %d", coreloop.MaxRounds, len(launcher.Launches()))
	}

	rec, _, _ := store.GetTask(context.Background(), "task-1")
	if rec.State.Status != domain.TaskStatusStopEscalate {
		t.Fatalf("expected persisted status stop_escalate, got %q", rec.State.Status)
	}
}

// TestRunner_BlockingVsAdvisoryTransitionAcrossRounds mirrors what used to
// be coreloop.Run's own aggregate/evaluate continuation test: a blocking
// finding in round 1 keeps the loop going; once the same step becomes
// advisory-only (round 2), the task reaches stop-success.
func TestRunner_BlockingVsAdvisoryTransitionAcrossRounds(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			round, _ := req.Facts["round"].(int)
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: round == 1}},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, &fakes.FakeAuditWriter{}))

	_, done, err := runner.Start(context.Background(), domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-done
	if result.Err != nil {
		t.Fatalf("result error: %v", result.Err)
	}

	if result.RoundsRun != 2 {
		t.Fatalf("expected the loop to continue past round 1 (blocking) and stop at round 2 (advisory only), got RoundsRun=%d", result.RoundsRun)
	}
	if result.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success once the finding becomes advisory-only, got %v (%s)", result.Outcome, result.Reason)
	}
}

// TestRunner_AuditsTaskOutcomeAfterMultipleRounds proves Runner writes
// exactly one "task-outcome" row per task, after every round's own
// "pdp-decision"/step rows, with a valid hash chain spanning the whole
// multi-round task — the piece coreloop.Run used to own internally that
// moved to this package.
func TestRunner_AuditsTaskOutcomeAfterMultipleRounds(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			round, _ := req.Facts["round"].(int)
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: round == 1}},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	auditWriter := &fakes.FakeAuditWriter{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, auditWriter))

	_, done, err := runner.Start(context.Background(), domain.Task{ID: "task-audit-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result := <-done; result.Err != nil {
		t.Fatalf("result error: %v", result.Err)
	}

	chain, err := auditWriter.GetTaskChain(context.Background(), "task-audit-1")
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	// round 1: pdp-decision + step ; round 2: pdp-decision + step ; + task-outcome.
	if len(chain) != 5 {
		t.Fatalf("expected 5 audit rows, got %d: %+v", len(chain), chain)
	}
	last := chain[len(chain)-1]
	if last.StepType != "task-outcome" || last.Status != string(coreloop.OutcomeStopSuccess) {
		t.Fatalf("expected the last row to be task-outcome/stop-success, got %+v", last)
	}
	if err := audit.VerifyChain(chain); err != nil {
		t.Fatalf("expected an intact hash chain, got: %v", err)
	}
}

// TestRunner_ReconcileTask_SkipsCompletedResolvesLaunchedDispatchesMissing
// is design §9's explicitly named test: a task left status=running with a
// mix of no-row / launched / completed step_runs resumes correctly without
// double-launching an in-flight step.
func TestRunner_ReconcileTask_SkipsCompletedResolvesLaunchedDispatchesMissing(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			if invocationID == "task-1:1:step-launched" {
				return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": false}`)}, nil
			}
			return launcherclient.LogBundle{}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "step-completed", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: true},
					{StepID: "step-launched", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: true},
					{StepID: "step-new", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseReview, Blocking: true},
				},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, &fakes.FakeAuditWriter{}))
	ctx := context.Background()
	task := domain.Task{ID: "task-1"}

	// Simulate a task a prior control-plane process crashed while running,
	// mid-round-1: task row exists (status=running), round 1 started, and
	// step_runs has one completed row, one still-launched row, and no row
	// at all for the third step.
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := store.EnsureRound(ctx, task.ID, 1); err != nil {
		t.Fatalf("EnsureRound: %v", err)
	}
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-completed", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1:1:step-completed"); err != nil {
		t.Fatalf("RecordStepLaunched(step-completed): %v", err)
	}
	if err := store.CompleteStepRun(ctx, task.ID, "task-1:1:step-completed", domain.StepRun{HasFinding: false}, nil); err != nil {
		t.Fatalf("CompleteStepRun(step-completed): %v", err)
	}
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-launched", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1:1:step-launched"); err != nil {
		t.Fatalf("RecordStepLaunched(step-launched): %v", err)
	}
	// step-new: deliberately no row at all yet.

	result, err := runner.ReconcileTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("ReconcileTask: %v", err)
	}
	if result.Err != nil {
		t.Fatalf("ReconcileTask result error: %v", result.Err)
	}
	if result.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success (no step reports a blocking finding), got %v (%s)", result.Outcome, result.Reason)
	}

	launches := launcher.Launches()
	if len(launches) != 1 || launches[0].StepID != "step-new" {
		t.Fatalf("expected exactly one (re)launch, for step-new only — step-completed must not be redispatched and step-launched must be resolved via ExtractLogs, not relaunched; got %+v", launches)
	}

	stepRuns, err := store.ListStepRuns(ctx, task.ID, 1)
	if err != nil {
		t.Fatalf("ListStepRuns: %v", err)
	}
	statuses := map[string]string{}
	for _, sr := range stepRuns {
		statuses[sr.StepID] = sr.Status
	}
	for _, id := range []string{"step-completed", "step-launched", "step-new"} {
		if statuses[id] != domain.StepRunStatusCompleted {
			t.Fatalf("expected %s to end up completed, got %q (all: %+v)", id, statuses[id], statuses)
		}
	}
}

// TestRunner_ReconcileTask_UnresolvableLaunchedStepEscalates exercises the
// fail-closed path: a still-"launched" step the launcher no longer knows
// about (e.g. launcher-mock itself also restarted) must not be silently
// relaunched or silently ignored — the task escalates instead.
func TestRunner_ReconcileTask_UnresolvableLaunchedStepEscalates(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{}, context.DeadlineExceeded // any error simulates "unresolvable"
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "step-launched", Type: executor.StepTypeAgentStage, Blocking: true}},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	auditWriter := &fakes.FakeAuditWriter{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, auditWriter))
	ctx := context.Background()
	task := domain.Task{ID: "task-1"}

	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := store.EnsureRound(ctx, task.ID, 1); err != nil {
		t.Fatalf("EnsureRound: %v", err)
	}
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-launched", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1:1:step-launched"); err != nil {
		t.Fatalf("RecordStepLaunched: %v", err)
	}

	result, err := runner.ReconcileTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("ReconcileTask: %v", err)
	}
	if result.Outcome != coreloop.OutcomeStopEscalate {
		t.Fatalf("expected stop-escalate on an unresolvable launched step, got %v", result.Outcome)
	}
	if len(launcher.Launches()) != 0 {
		t.Fatalf("expected zero (re)launches — must not blindly relaunch an unresolvable step, got %d", len(launcher.Launches()))
	}

	rec, _, _ := store.GetTask(ctx, task.ID)
	if rec.State.Status != domain.TaskStatusStopEscalate {
		t.Fatalf("expected persisted status stop_escalate, got %q", rec.State.Status)
	}
}

// TestRunner_ReconcileTask_NonRunningTaskIsNoop guards against Reconcile
// racing a task that already reached a terminal state some other way, or
// against a stale/unknown task ID — either must be a no-op, not an error.
func TestRunner_ReconcileTask_NonRunningTaskIsNoop(t *testing.T) {
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(&fakes.FakeContainerLauncher{}, &fakes.FakePolicyClient{}, store, &fakes.FakeAuditWriter{}))

	result, err := runner.ReconcileTask(context.Background(), "no-such-task")
	if err != nil {
		t.Fatalf("ReconcileTask on unknown task: %v", err)
	}
	if result.Outcome != "" {
		t.Fatalf("expected a no-op Result for an unknown task, got %+v", result)
	}
}
