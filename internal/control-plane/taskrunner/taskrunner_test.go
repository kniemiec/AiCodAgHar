package taskrunner_test

import (
	"context"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
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
			if invocationID == "task-1-r1-step-launched" {
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
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-completed", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1-r1-step-completed"); err != nil {
		t.Fatalf("RecordStepLaunched(step-completed): %v", err)
	}
	if err := store.CompleteStepRun(ctx, task.ID, "task-1-r1-step-completed", domain.StepRun{HasFinding: false}, nil); err != nil {
		t.Fatalf("CompleteStepRun(step-completed): %v", err)
	}
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-launched", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1-r1-step-launched"); err != nil {
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

// TestRunner_ReconcileTask_ResumesRoundAfterLastCompleted guards against a
// regression of the bug where ReconcileTask resumed from
// TaskState.CurrentRound instead of CurrentRound+1: CurrentRound records the
// last *completed* round, so a task that crashed while dispatching the round
// after it must reconcile that next round — not re-inspect the
// already-finished one (whose step_runs are all "completed" already) while
// silently ignoring the round actually in flight, which would otherwise get
// blindly re-dispatched from scratch by the normal loop.
func TestRunner_ReconcileTask_ResumesRoundAfterLastCompleted(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
			if invocationID == "task-1-r2-step-1" {
				return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": false}`)}, nil
			}
			return launcherclient.LogBundle{}, nil
		},
	}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision:      "allow",
				ResolvedSteps: []policyclient.ResolvedStep{{StepID: "step-1", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: true}},
			}, nil
		},
	}
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(launcher, policy, store, &fakes.FakeAuditWriter{}))
	ctx := context.Background()
	task := domain.Task{ID: "task-1"}

	// Round 1 ran to completion with an outstanding blocking finding (a
	// normal "continue" round) before the process crashed while dispatching
	// round 2.
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := store.EnsureRound(ctx, task.ID, 1); err != nil {
		t.Fatalf("EnsureRound(1): %v", err)
	}
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-1", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1-r1-step-1"); err != nil {
		t.Fatalf("RecordStepLaunched(round 1): %v", err)
	}
	if err := store.CompleteStepRun(ctx, task.ID, "task-1-r1-step-1", domain.StepRun{HasFinding: true}, nil); err != nil {
		t.Fatalf("CompleteStepRun(round 1): %v", err)
	}
	if err := store.UpdateTaskState(ctx, domain.TaskState{TaskID: task.ID, Status: domain.TaskStatusRunning, CurrentRound: 1}); err != nil {
		t.Fatalf("UpdateTaskState: %v", err)
	}

	// Round 2 started (its step launched) but never resolved before the crash.
	if err := store.EnsureRound(ctx, task.ID, 2); err != nil {
		t.Fatalf("EnsureRound(2): %v", err)
	}
	if err := store.RecordStepLaunched(ctx, task.ID, 2, policyclient.ResolvedStep{StepID: "step-1", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1-r2-step-1"); err != nil {
		t.Fatalf("RecordStepLaunched(round 2): %v", err)
	}

	result, err := runner.ReconcileTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("ReconcileTask: %v", err)
	}
	if result.Err != nil {
		t.Fatalf("ReconcileTask result error: %v", result.Err)
	}
	if result.RoundsRun != 2 {
		t.Fatalf("expected reconciliation to resume at round 2 (round 1 already completed), got RoundsRun=%d", result.RoundsRun)
	}
	if result.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success (round 2's step reports no blocking finding), got %v (%s)", result.Outcome, result.Reason)
	}
	if launches := launcher.Launches(); len(launches) != 0 {
		t.Fatalf("expected no (re)launches — round 1's step is already completed and round 2's step must be resolved via ExtractLogs, not relaunched; got %+v", launches)
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
	if err := store.RecordStepLaunched(ctx, task.ID, 1, policyclient.ResolvedStep{StepID: "step-launched", Type: executor.StepTypeAgentStage, Blocking: true}, "task-1-r1-step-launched"); err != nil {
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

// approvePhasePolicy resolves an approve-phase gate plus a publish-phase
// step that only ever dispatches once that gate is satisfied — the shape
// design §4.4's approve tests below share.
func approvePhasePolicy() *fakes.FakePolicyClient {
	return &fakes.FakePolicyClient{
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
}

// TestRunner_ApprovePhase_PausesThenResumesOnApproval exercises design
// §4.4's full pause/resume cycle end to end: an unmet approve gate persists
// waiting_approval with an issue number and leaves current_round at the
// last *completed* round (0 here, since round 1 never completes) —
// ReconcileTask/Resume's own "+1" convention depends on that not changing.
// RecordApproval + Resume then continues the same round to publish and
// stop-success, using approval.RealApprovalVerifier (not the scripted fake)
// since this specifically exercises reading back what RecordApproval wrote.
func TestRunner_ApprovePhase_PausesThenResumesOnApproval(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	git := &fakes.FakeGitHostClient{}
	store := &fakes.FakeTaskStore{}
	deps := newDeps(launcher, approvePhasePolicy(), store, &fakes.FakeAuditWriter{})
	deps.Coreloop.IssueOpener = git
	deps.Coreloop.Approval = &approval.RealApprovalVerifier{Reader: taskrunner.NewTaskApprovalReader(store)}
	runner := taskrunner.NewRunner(deps)
	ctx := context.Background()

	taskID, done, err := runner.Start(ctx, domain.Task{ID: "task-1", TargetRepo: "acme/widgets"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-done
	if result.Outcome != coreloop.OutcomeWaitingApproval {
		t.Fatalf("expected waiting-approval, got %v (%s)", result.Outcome, result.Reason)
	}
	if len(launcher.Launches()) != 0 {
		t.Fatalf("expected zero launches before approval, got %d", len(launcher.Launches()))
	}

	rec, ok, err := store.GetTask(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if rec.State.Status != domain.TaskStatusWaitingApproval {
		t.Fatalf("expected persisted status waiting_approval, got %q", rec.State.Status)
	}
	if rec.State.ApprovalIssueNumber == nil || *rec.State.ApprovalIssueNumber == 0 {
		t.Fatalf("expected a non-zero persisted approval_issue_number, got %v", rec.State.ApprovalIssueNumber)
	}
	if rec.State.CurrentRound != 0 {
		t.Fatalf("expected current_round to stay 0 (round 1 never completed), got %d", rec.State.CurrentRound)
	}

	if err := runner.RecordApproval(ctx, taskID, "alice", "issue labeled approved by alice"); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	rec, _, _ = store.GetTask(ctx, taskID)
	if rec.State.Status != domain.TaskStatusRunning || rec.State.ApprovedBy != "alice" {
		t.Fatalf("expected status=running, approved_by=alice after RecordApproval, got %+v", rec.State)
	}

	result2, err := runner.Resume(ctx, taskID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result2.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("expected stop-success after approval, got %v (%s)", result2.Outcome, result2.Reason)
	}
	if launches := launcher.Launches(); len(launches) != 1 || launches[0].StepID != "notify" {
		t.Fatalf("expected exactly one launch, for the publish-phase step, got %+v", launches)
	}

	rec, _, _ = store.GetTask(ctx, taskID)
	if rec.State.Status != domain.TaskStatusStopSuccess {
		t.Fatalf("expected final persisted status stop_success, got %q", rec.State.Status)
	}
}

// TestRunner_ApprovePhase_RejectionEscalatesWithoutResuming exercises
// RecordRejection's whole terminal path: unlike approval, a rejection never
// gets to Resume — it's stop-escalate on its own.
func TestRunner_ApprovePhase_RejectionEscalatesWithoutResuming(t *testing.T) {
	launcher := &fakes.FakeContainerLauncher{}
	store := &fakes.FakeTaskStore{}
	deps := newDeps(launcher, approvePhasePolicy(), store, &fakes.FakeAuditWriter{})
	deps.Coreloop.IssueOpener = &fakes.FakeGitHostClient{}
	runner := taskrunner.NewRunner(deps)
	ctx := context.Background()

	taskID, done, err := runner.Start(ctx, domain.Task{ID: "task-1", TargetRepo: "acme/widgets"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result := <-done; result.Outcome != coreloop.OutcomeWaitingApproval {
		t.Fatalf("expected waiting-approval, got %v", result.Outcome)
	}

	if err := runner.RecordRejection(ctx, taskID, "bob", "issue labeled rejected by bob"); err != nil {
		t.Fatalf("RecordRejection: %v", err)
	}

	rec, ok, err := store.GetTask(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if rec.State.Status != domain.TaskStatusStopEscalate {
		t.Fatalf("expected persisted status stop_escalate, got %q", rec.State.Status)
	}
	if rec.State.Reason == "" {
		t.Fatal("expected a non-empty persisted reason")
	}
	if len(launcher.Launches()) != 0 {
		t.Fatalf("expected zero launches (never resumed), got %d", len(launcher.Launches()))
	}
}

// TestRunner_RecordApproval_RejectsNonWaitingTask guards RecordApproval and
// RecordRejection against a stale/duplicate webhook delivery arriving after
// the task already moved on (a second "approved" label event on an issue
// whose task already resumed and finished, say) — both must fail rather
// than silently reopening or double-processing it.
func TestRunner_RecordApproval_RejectsNonWaitingTask(t *testing.T) {
	store := &fakes.FakeTaskStore{}
	runner := taskrunner.NewRunner(newDeps(&fakes.FakeContainerLauncher{}, &fakes.FakePolicyClient{}, store, &fakes.FakeAuditWriter{}))
	ctx := context.Background()
	task := domain.Task{ID: "task-1"}
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := runner.RecordApproval(ctx, task.ID, "alice", "evidence"); err == nil {
		t.Fatal("expected RecordApproval to fail for a task that isn't waiting_approval")
	}
	if err := runner.RecordRejection(ctx, task.ID, "bob", "evidence"); err == nil {
		t.Fatal("expected RecordRejection to fail for a task that isn't waiting_approval")
	}
}
