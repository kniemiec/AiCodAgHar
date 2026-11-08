package taskrunner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
)

var errBoom = errors.New("boom")

// Failure-injecting wrappers around the fakes: the fakes themselves stay
// plain, each test row overrides exactly one method.

type failingStore struct {
	*fakes.FakeTaskStore
	recordLaunched func() error
	completeStep   func() error
	ensureRound    func() error
	updateState    func(domain.TaskState) error
	setPRNumber    func() error
}

func (s *failingStore) RecordStepLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, inv string) error {
	if s.recordLaunched != nil {
		return s.recordLaunched()
	}
	return s.FakeTaskStore.RecordStepLaunched(ctx, taskID, round, step, inv)
}

func (s *failingStore) CompleteStepRun(ctx context.Context, taskID, inv string, run domain.StepRun, execErr error) error {
	if s.completeStep != nil {
		return s.completeStep()
	}
	return s.FakeTaskStore.CompleteStepRun(ctx, taskID, inv, run, execErr)
}

func (s *failingStore) EnsureRound(ctx context.Context, taskID string, round int) (bool, error) {
	if s.ensureRound != nil {
		return false, s.ensureRound()
	}
	return s.FakeTaskStore.EnsureRound(ctx, taskID, round)
}

func (s *failingStore) UpdateTaskState(ctx context.Context, st domain.TaskState, ev ...domain.TaskEvent) error {
	if s.updateState != nil {
		if err := s.updateState(st); err != nil {
			return err
		}
	}
	return s.FakeTaskStore.UpdateTaskState(ctx, st, ev...)
}

func (s *failingStore) SetTaskPRNumber(ctx context.Context, taskID string, n int) error {
	if s.setPRNumber != nil {
		return s.setPRNumber()
	}
	return s.FakeTaskStore.SetTaskPRNumber(ctx, taskID, n)
}

type failingGit struct {
	*fakes.FakeGitHostClient
	branchErr, issueErr error
}

func (g *failingGit) CreateOrReuseBranch(ctx context.Context, repo gitclient.RepoRef, branch, base string) (gitclient.Branch, error) {
	if g.branchErr != nil {
		return gitclient.Branch{}, g.branchErr
	}
	return g.FakeGitHostClient.CreateOrReuseBranch(ctx, repo, branch, base)
}

func (g *failingGit) CreateOrReuseIssue(ctx context.Context, repo gitclient.RepoRef, title, body string) (gitclient.Issue, error) {
	if g.issueErr != nil {
		return gitclient.Issue{}, g.issueErr
	}
	return g.FakeGitHostClient.CreateOrReuseIssue(ctx, repo, title, body)
}

type failingSecrets struct {
	*fakes.FakeSecretsClient
	issueErr error
}

func (s *failingSecrets) IssueCredential(ctx context.Context, req secretsclient.CredentialRequest) (secretsclient.Credential, error) {
	if s.issueErr != nil {
		return secretsclient.Credential{}, s.issueErr
	}
	return s.FakeSecretsClient.IssueCredential(ctx, req)
}

// failingAudit fails writes for one step type ("" = none).
type failingAudit struct {
	*fakes.FakeAuditWriter
	stepType string
}

func (a *failingAudit) WriteInvocationRecord(ctx context.Context, rec audit.InvocationAuditRecord) error {
	if a.stepType != "" && rec.StepType == a.stepType {
		return errBoom
	}
	return a.FakeAuditWriter.WriteInvocationRecord(ctx, rec)
}

// env is one failure-injection scenario's wiring.
type env struct {
	launcher *fakes.FakeContainerLauncher
	policy   *fakes.FakePolicyClient
	store    *failingStore
	git      *failingGit
	secrets  *failingSecrets
	audit    *failingAudit
}

func newEnv(steps ...policyclient.ResolvedStep) *env {
	return &env{
		launcher: &fakes.FakeContainerLauncher{},
		policy: &fakes.FakePolicyClient{EvaluateFunc: func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: steps}, nil
		}},
		store:   &failingStore{FakeTaskStore: &fakes.FakeTaskStore{}},
		git:     &failingGit{FakeGitHostClient: &fakes.FakeGitHostClient{}},
		secrets: &failingSecrets{FakeSecretsClient: &fakes.FakeSecretsClient{}},
		audit:   &failingAudit{FakeAuditWriter: &fakes.FakeAuditWriter{}},
	}
}

func (e *env) runner() *taskrunner.Runner {
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: e.launcher, Secrets: e.secrets, Audit: e.audit},
		&executor.DeterministicStepExecutor{Launcher: e.launcher, Secrets: e.secrets, Audit: e.audit},
	)
	return taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy: e.policy, Approval: &fakes.FakeApprovalVerifier{}, Audit: e.audit, Executors: registry,
			IssueOpener: e.git, PRCreator: e.git,
		},
		Store:    e.store,
		Launcher: e.launcher,
	})
}

func (e *env) start(t *testing.T, r *taskrunner.Runner) taskrunner.Result {
	t.Helper()
	_, done, err := r.Start(context.Background(), domain.Task{ID: "task-1", TargetRepo: "acme/widgets"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return <-done
}

func agentStep(id, phase string) policyclient.ResolvedStep {
	return policyclient.ResolvedStep{StepID: id, Type: executor.StepTypeAgentStage, Phase: phase}
}

// TestRunner_FailureTable is the ER-1 table test: every failure path in the
// design's §2 table that this PR can reach ends the task failed with the
// listed class/retryable, exactly one task.failed event, and a restart never
// picks it up again. (The launcher-outcome rows arrive with the launcher
// outcome contract.)
func TestRunner_FailureTable(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(e *env)
		steps     []policyclient.ResolvedStep
		class     string
		retryable bool
		dep       string
	}{
		{"pdp evaluate error", func(e *env) {
			e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
				return policyclient.PolicyDecision{}, errBoom
			}
		}, nil, "dependency", true, "pdp"},
		{"unknown pdp decision", func(e *env) {
			e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
				return policyclient.PolicyDecision{Decision: "maybe"}, nil
			}
		}, nil, "platform", false, ""},
		{"audit pdp decision", func(e *env) { e.audit.stepType = "pdp-decision" },
			nil, "platform", false, ""},
		{"audit step", func(e *env) { e.audit.stepType = executor.StepTypeAgentStage },
			[]policyclient.ResolvedStep{agentStep("review", "")}, "platform", false, ""},
		{"audit task outcome", func(e *env) { e.audit.stepType = "task-outcome" },
			nil, "platform", false, ""},
		{"branch creation", func(e *env) { e.git.branchErr = errBoom },
			[]policyclient.ResolvedStep{agentStep("implement", policyclient.PhaseGenerate)}, "dependency", true, "git_host"},
		{"approval issue creation", func(e *env) { e.git.issueErr = errBoom },
			[]policyclient.ResolvedStep{{StepID: "signoff", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseApprove}}, "dependency", true, "git_host"},
		{"credential mint", func(e *env) { e.secrets.issueErr = errBoom },
			[]policyclient.ResolvedStep{agentStep("review", "")}, "dependency", true, "secrets_issuer"},
		{"launch unavailable", func(e *env) {
			e.launcher.LaunchFunc = func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
				return launcherclient.RunResult{}, status.Error(codes.Unavailable, "down")
			}
		}, []policyclient.ResolvedStep{agentStep("review", "")}, "dependency", true, "launcher"},
		{"launch internal (boot failure)", func(e *env) {
			e.launcher.LaunchFunc = func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
				return launcherclient.RunResult{}, status.Error(codes.Internal, "boot failed")
			}
		}, []policyclient.ResolvedStep{agentStep("review", "")}, "infrastructure", true, ""},
		{"launch invalid argument", func(e *env) {
			e.launcher.LaunchFunc = func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
				return launcherclient.RunResult{}, status.Error(codes.InvalidArgument, "bad payload_ref")
			}
		}, []policyclient.ResolvedStep{agentStep("review", "")}, "platform", false, ""},
		{"extract logs", func(e *env) {
			e.launcher.ExtractLogsFunc = func(context.Context, string) (launcherclient.LogBundle, error) {
				return launcherclient.LogBundle{}, errBoom
			}
		}, []policyclient.ResolvedStep{agentStep("review", "")}, "dependency", true, "launcher"},
		{"unparseable structured result", func(e *env) {
			e.launcher.ExtractLogsFunc = func(context.Context, string) (launcherclient.LogBundle, error) {
				return launcherclient.LogBundle{StructuredResultJSON: []byte("{not json")}, nil
			}
		}, []policyclient.ResolvedStep{agentStep("review", "")}, "agent", false, ""},
		{"record launched", func(e *env) { e.store.recordLaunched = func() error { return errBoom } },
			[]policyclient.ResolvedStep{agentStep("review", "")}, "platform", false, ""},
		{"record completed", func(e *env) { e.store.completeStep = func() error { return errBoom } },
			[]policyclient.ResolvedStep{agentStep("review", "")}, "platform", false, ""},
		{"ensure round", func(e *env) { e.store.ensureRound = func() error { return errBoom } },
			nil, "platform", false, ""},
		{"update task state", func(e *env) {
			e.store.updateState = func(st domain.TaskState) error {
				if st.Status == domain.TaskStatusStopSuccess {
					return errBoom
				}
				return nil
			}
		}, nil, "platform", false, ""},
		{"set pr number", func(e *env) { e.store.setPRNumber = func() error { return errBoom } },
			[]policyclient.ResolvedStep{agentStep("implement", policyclient.PhaseGenerate)}, "platform", false, ""},
		{"no executor for step type", func(e *env) {},
			[]policyclient.ResolvedStep{{StepID: "x", Type: "mystery-step", Phase: policyclient.PhaseReview}}, "platform", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(tc.steps...)
			tc.setup(e)
			r := e.runner()
			res := e.start(t, r)
			if res.Err == nil {
				t.Fatalf("expected an error result, got %+v", res)
			}

			rec, _, _ := e.store.GetTask(context.Background(), "task-1")
			if rec.State.Status != domain.TaskStatusFailed {
				t.Fatalf("status = %q, want failed", rec.State.Status)
			}
			if rec.State.ErrorClass != tc.class || rec.State.Retryable != tc.retryable {
				t.Fatalf("class/retryable = %s/%v, want %s/%v", rec.State.ErrorClass, rec.State.Retryable, tc.class, tc.retryable)
			}
			if tc.retryable == (rec.State.FinishedAt != nil) {
				t.Fatalf("finished_at set = %v, want set only for terminal failures", rec.State.FinishedAt != nil)
			}
			if len(rec.State.Reason) == 0 || len(rec.State.Reason) > 512 {
				t.Fatalf("reason length %d not in (0,512]", len(rec.State.Reason))
			}

			var failed []domain.TaskEvent
			for _, ev := range e.store.Events("task-1") {
				if ev.Type == domain.EventTaskFailed {
					failed = append(failed, ev)
				}
			}
			if len(failed) != 1 {
				t.Fatalf("task.failed events = %d, want 1", len(failed))
			}
			if got := failed[0].Payload["error_class"]; got != tc.class {
				t.Fatalf("event error_class = %v, want %s", got, tc.class)
			}
			if tc.dep != "" && failed[0].Payload["dependency"] != tc.dep {
				t.Fatalf("event dependency = %v, want %s", failed[0].Payload["dependency"], tc.dep)
			}
			if _, has := failed[0].Payload["wall_ms"]; has == tc.retryable {
				t.Fatalf("wall_ms present = %v, want only for terminal failures", has)
			}

			// A restart never resumes a failed task.
			launches := len(e.launcher.Launches())
			if _, err := r.ReconcileTask(context.Background(), "task-1"); err != nil {
				t.Fatalf("ReconcileTask on failed task: %v", err)
			}
			if len(e.launcher.Launches()) != launches {
				t.Fatalf("reconcile relaunched a failed task")
			}
			ids, _ := e.store.ListRunningTaskIDs(context.Background())
			if len(ids) != 0 {
				t.Fatalf("failed task listed as running: %v", ids)
			}

			// Retry only resumes retryable failures.
			_, err := r.StartRetry(context.Background(), "task-1", "op", "127.0.0.1:1")
			if tc.retryable && err != nil {
				t.Fatalf("StartRetry on retryable failure: %v", err)
			}
			if !tc.retryable && !errors.Is(err, taskrunner.ErrNotRetryable) {
				t.Fatalf("StartRetry on terminal failure = %v, want ErrNotRetryable", err)
			}
		})
	}
}

// TestRunner_Retry covers the operator-retry compare-and-set: a retryable
// failure resumes and can complete, retry_count increments, task.retried is
// recorded, and exhausting TASK_MAX_RETRIES makes it non-retryable.
func TestRunner_Retry(t *testing.T) {
	fail := true
	e := newEnv(agentStep("review", ""))
	e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		if fail {
			return policyclient.PolicyDecision{}, errBoom
		}
		return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: []policyclient.ResolvedStep{agentStep("review", "")}}, nil
	}
	r := e.runner()
	ctx := context.Background()

	if res := e.start(t, r); res.Err == nil {
		t.Fatal("expected first run to fail")
	}

	fail = false
	res, err := r.Retry(ctx, "task-1", "alice", "10.0.0.1:5555")
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if res.Outcome != coreloop.OutcomeStopSuccess {
		t.Fatalf("retry outcome = %v (%v)", res.Outcome, res.Err)
	}
	rec, _, _ := e.store.GetTask(ctx, "task-1")
	if rec.State.Status != domain.TaskStatusStopSuccess || rec.State.RetryCount != 1 || rec.State.ErrorClass != "" {
		t.Fatalf("after retry: %+v", rec.State)
	}
	var retried domain.TaskEvent
	for _, ev := range e.store.Events("task-1") {
		if ev.Type == domain.EventTaskRetried {
			retried = ev
		}
	}
	if retried.Payload["triggered_by"] != "alice" || retried.Payload["remote_addr"] != "10.0.0.1:5555" || retried.Payload["previous_class"] != "dependency" {
		t.Fatalf("task.retried payload = %v", retried.Payload)
	}

	// Not failed any more: second retry is refused; unknown task is distinct.
	if _, err := r.StartRetry(ctx, "task-1", "alice", ""); !errors.Is(err, taskrunner.ErrNotRetryable) {
		t.Fatalf("retry of finished task = %v", err)
	}
	if _, err := r.StartRetry(ctx, "nope", "alice", ""); !errors.Is(err, taskrunner.ErrNoSuchTask) {
		t.Fatalf("retry of unknown task = %v", err)
	}
}

func TestRunner_RetryExhausted(t *testing.T) {
	e := newEnv()
	e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		return policyclient.PolicyDecision{}, errBoom
	}
	r := e.runner() // default max retries = 3
	ctx := context.Background()
	if res := e.start(t, r); res.Err == nil {
		t.Fatal("expected failure")
	}
	for i := 1; i <= taskrunner.DefaultMaxRetries; i++ {
		res, err := r.Retry(ctx, "task-1", "op", "")
		if err != nil || res.Err == nil {
			t.Fatalf("retry %d: res=%+v err=%v (should run and fail again)", i, res, err)
		}
	}
	if _, err := r.StartRetry(ctx, "task-1", "op", ""); !errors.Is(err, taskrunner.ErrNotRetryable) {
		t.Fatalf("retry beyond the bound = %v, want ErrNotRetryable", err)
	}
}

// TestRunner_PolicyOutcomesAreNotFailures is ER-3: PDP deny, rejected
// approval and the round cap stay stop_escalate with a reason_kind and never
// write task.failed.
func TestRunner_PolicyOutcomesAreNotFailures(t *testing.T) {
	t.Run("pdp deny", func(t *testing.T) {
		e := newEnv()
		e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{Decision: "deny", Reason: "no"}, nil
		}
		res := e.start(t, e.runner())
		assertEscalated(t, e, res, domain.ReasonPolicyDenied)
	})
	t.Run("round cap", func(t *testing.T) {
		e := newEnv(policyclient.ResolvedStep{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: true})
		e.launcher.ExtractLogsFunc = func(context.Context, string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		}
		res := e.start(t, e.runner())
		assertEscalated(t, e, res, domain.ReasonRoundCap)
	})
	t.Run("invalid phase config", func(t *testing.T) {
		e := newEnv(agentStep("a", policyclient.PhaseGenerate), agentStep("b", policyclient.PhaseGenerate))
		res := e.start(t, e.runner())
		assertEscalated(t, e, res, domain.ReasonInvalidPhaseConfig)
	})
	t.Run("approval rejected", func(t *testing.T) {
		e := newEnv(policyclient.ResolvedStep{StepID: "signoff", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseApprove})
		r := e.runner()
		res := e.start(t, r)
		if res.Outcome != coreloop.OutcomeWaitingApproval {
			t.Fatalf("outcome = %v", res.Outcome)
		}
		if err := r.RecordRejection(context.Background(), "task-1", "bob", "labeled rejected"); err != nil {
			t.Fatalf("RecordRejection: %v", err)
		}
		rec, _, _ := e.store.GetTask(context.Background(), "task-1")
		if rec.State.Status != domain.TaskStatusStopEscalate || rec.State.FinishedAt == nil {
			t.Fatalf("state = %+v", rec.State)
		}
		finished := lastEvent(e.store.Events("task-1"), domain.EventTaskFinished)
		if finished.Payload["reason_kind"] != domain.ReasonApprovalRejected || finished.Payload["outcome"] != "escalated" {
			t.Fatalf("task.finished = %v", finished.Payload)
		}
		if lastEvent(e.store.Events("task-1"), domain.EventTaskFailed).Type != "" {
			t.Fatal("rejection wrote task.failed")
		}
	})
}

func assertEscalated(t *testing.T, e *env, res taskrunner.Result, kind string) {
	t.Helper()
	if res.Err != nil || res.Outcome != coreloop.OutcomeStopEscalate || res.ReasonKind != kind {
		t.Fatalf("result = %+v, want stop-escalate/%s", res, kind)
	}
	rec, _, _ := e.store.GetTask(context.Background(), "task-1")
	if rec.State.Status != domain.TaskStatusStopEscalate {
		t.Fatalf("status = %q", rec.State.Status)
	}
	events := e.store.Events("task-1")
	if lastEvent(events, domain.EventTaskFailed).Type != "" {
		t.Fatal("policy outcome wrote task.failed")
	}
	if got := lastEvent(events, domain.EventTaskFinished).Payload["reason_kind"]; got != kind {
		t.Fatalf("task.finished reason_kind = %v, want %s", got, kind)
	}
}

func lastEvent(events []domain.TaskEvent, typ string) domain.TaskEvent {
	var out domain.TaskEvent
	for _, e := range events {
		if e.Type == typ {
			out = e
		}
	}
	return out
}

// TestRunner_EventSequence is TS-2: a two-round, approval-gated task leaves
// the §3 story in order.
func TestRunner_EventSequence(t *testing.T) {
	e := newEnv()
	round := 0
	e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		round++
		return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: []policyclient.ResolvedStep{
			{StepID: "review", Type: executor.StepTypeAgentStage, Blocking: round == 1, Phase: policyclient.PhaseReview},
			{StepID: "signoff", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseApprove},
			{StepID: "publish", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhasePublish},
		}}, nil
	}
	e.launcher.ExtractLogsFunc = func(_ context.Context, inv string) (launcherclient.LogBundle, error) {
		if inv == "task-1-r1-review" {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": true}`)}, nil
		}
		return launcherclient.LogBundle{}, nil
	}
	r := e.runner()
	ctx := context.Background()
	// Real verifier so RecordApproval is what unblocks the gate.
	e.start(t, r) // round 1 (continue) + round 2 (waiting approval)
	rec, _, _ := e.store.GetTask(ctx, "task-1")
	if rec.State.Status != domain.TaskStatusWaitingApproval {
		t.Fatalf("status = %q, want waiting_approval", rec.State.Status)
	}
	if err := r.RecordApproval(ctx, "task-1", "alice", "ok"); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	got := e.store.EventTypes("task-1")
	want := []string{
		"task.accepted",
		"round.started", "policy.decided", "phase.started", "step.launched", "step.resolved", "phase.finished", "round.finished",
		"round.started", "policy.decided", "phase.started", "step.launched", "step.resolved", "phase.finished", "approval.requested",
		"approval.granted",
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s\nall: %v", i, got[i], want[i], got)
		}
	}
	for _, ev := range e.store.Events("task-1") {
		if ev.ID == 0 || ev.At.IsZero() {
			t.Fatalf("event %s not stamped by store", ev.Type)
		}
	}
}

// TestRunner_EventFailureRollsBackState is TS-3 against the fake: a failing
// event append leaves the state change unapplied.
func TestRunner_EventFailureRollsBackState(t *testing.T) {
	store := &fakes.FakeTaskStore{}
	ctx := context.Background()
	if err := store.CreateTask(ctx, domain.Task{ID: "t"}); err != nil {
		t.Fatal(err)
	}
	store.FailEventAppend = func(e domain.TaskEvent) error { return errBoom }
	err := store.UpdateTaskState(ctx, domain.TaskState{TaskID: "t", Status: domain.TaskStatusStopSuccess, CurrentRound: 1},
		domain.TaskEvent{TaskID: "t", Type: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	rec, _, _ := store.GetTask(ctx, "t")
	if rec.State.Status != domain.TaskStatusRunning || rec.State.CurrentRound != 0 {
		t.Fatalf("state changed despite failed event: %+v", rec.State)
	}
}

func TestWaitActive(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	at := func(m int, typ string) domain.TaskEvent {
		return domain.TaskEvent{Type: typ, At: t0.Add(time.Duration(m) * time.Minute)}
	}
	events := []domain.TaskEvent{
		at(0, domain.EventTaskAccepted),
		at(10, domain.EventApprovalRequested),
		at(70, domain.EventApprovalGranted), // 60m human wait
		at(75, domain.EventTaskFailed),
		at(80, domain.EventTaskRetried), // 5m operator wait
	}
	wall, wait, active := domain.WaitActive(events, t0.Add(100*time.Minute))
	if wall != 100*time.Minute || wait != 65*time.Minute || active != 35*time.Minute {
		t.Fatalf("wall/wait/active = %v/%v/%v", wall, wait, active)
	}
}

// TestRunner_HeartbeatWhileStepInFlight is §4: a long step appends
// step.heartbeat; none are written once it resolves.
func TestRunner_HeartbeatWhileStepInFlight(t *testing.T) {
	e := newEnv(agentStep("review", ""))
	release := make(chan struct{})
	e.launcher.LaunchFunc = func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
		<-release
		return launcherclient.RunResult{InvocationID: req.InvocationID, Status: "completed"}, nil
	}
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: e.launcher, Secrets: e.secrets, Audit: e.audit},
		&executor.DeterministicStepExecutor{Launcher: e.launcher, Secrets: e.secrets, Audit: e.audit},
	)
	r := taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{Policy: e.policy, Approval: &fakes.FakeApprovalVerifier{}, Audit: e.audit, Executors: registry,
			HeartbeatInterval: 10 * time.Millisecond},
		Store: e.store, Launcher: e.launcher,
	})
	_, done, err := r.Start(context.Background(), domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	close(release)
	<-done

	n := 0
	for _, ev := range e.store.Events("task-1") {
		if ev.Type == domain.EventStepHeartbeat {
			n++
			if ev.StepID != "review" || ev.InvocationID == "" {
				t.Fatalf("heartbeat missing step identity: %+v", ev)
			}
		}
	}
	if n < 2 {
		t.Fatalf("heartbeats = %d, want >= 2", n)
	}
}
