package instrumented_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/metric"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/instrumented"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry/telemetrytest"
)

var errBoom = errors.New("boom")

func TestDecorators_RecordCallsAndPassErrorsThrough(t *testing.T) {
	reader := telemetrytest.New()
	m := telemetry.NewControlPlane(reader.Provider, telemetry.ControlPlaneOptions{})
	ctx := context.Background()

	policy := instrumented.Policy{Metrics: m, Next: &fakes.FakePolicyClient{EvaluateFunc: func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		return policyclient.PolicyDecision{}, errBoom
	}}}
	if _, err := policy.Evaluate(ctx, policyclient.PolicyRequest{}); err != errBoom {
		t.Fatalf("Policy.Evaluate err = %v, want the client's own error unchanged", err)
	}

	launcher := instrumented.Launcher{Metrics: m, Next: &fakes.FakeContainerLauncher{LaunchFunc: func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
		return launcherclient.RunResult{}, status.Error(codes.Unavailable, "down")
	}}}
	if _, err := launcher.Launch(ctx, launcherclient.LaunchRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Launcher.Launch err = %v", err)
	}
	if _, err := launcher.ExtractLogs(ctx, "inv"); err != nil {
		t.Fatalf("ExtractLogs: %v", err)
	}

	secrets := instrumented.Secrets{Metrics: m, Next: &fakes.FakeSecretsClient{}}
	if _, err := secrets.IssueCredential(ctx, secretsclient.CredentialRequest{InvocationID: "inv", BotAccount: "bot", TargetRepo: "acme/widgets"}); err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	store := instrumented.Store{Metrics: m, Next: &fakes.FakeTaskStore{}}
	if _, err := store.RetryTask(ctx, "missing", 3, domain.TaskEvent{}); err == nil {
		t.Fatal("RetryTask on a missing task succeeded")
	}

	for _, c := range []struct {
		match map[string]string
		want  float64
	}{
		{map[string]string{"dependency": "pdp", "method": "Evaluate", "outcome": "error"}, 1},
		{map[string]string{"dependency": "launcher", "method": "Launch", "outcome": "error", "code": "Unavailable"}, 1},
		{map[string]string{"dependency": "launcher", "method": "ExtractLogs", "outcome": "ok", "code": "OK"}, 1},
		{map[string]string{"dependency": "secrets_issuer", "method": "IssueCredential", "outcome": "ok"}, 1},
		{map[string]string{"dependency": "postgres", "method": "RetryTask"}, 1},
	} {
		if got := reader.Value(t, "harness_dependency_requests_total", c.match); got != c.want {
			t.Errorf("harness_dependency_requests_total%v = %v, want %v", c.match, got, c.want)
		}
	}
	if got := reader.Value(t, "harness_dependency_request_duration_seconds", map[string]string{"dependency": "pdp"}); got != 1 {
		t.Errorf("pdp latency observations = %v, want 1", got)
	}
}

// world is a runner wired the way main.go wires it — instrumented clients,
// fake store with the EventSink as its after-commit observer — over the
// fakes, on meter provider mp.
type world struct {
	runner   *taskrunner.Runner
	store    *fakes.FakeTaskStore
	launcher *fakes.FakeContainerLauncher
	secrets  *fakes.FakeSecretsClient
	metrics  *telemetry.ControlPlane
}

func newWorld(mp metric.MeterProvider) *world {
	m := telemetry.NewControlPlane(mp, telemetry.ControlPlaneOptions{})
	w := &world{
		store: &fakes.FakeTaskStore{Observer: m},
		launcher: &fakes.FakeContainerLauncher{ExtractLogsFunc: func(context.Context, string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": false, "note": "AGENT-OUTPUT-MARKER"}`)}, nil
		}},
		secrets: &fakes.FakeSecretsClient{},
		metrics: m,
	}
	launcher := instrumented.Launcher{Next: w.launcher, Metrics: m}
	secrets := instrumented.Secrets{Next: w.secrets, Metrics: m}
	audit := instrumented.Audit{Next: &fakes.FakeAuditWriter{}, Metrics: m}
	git := instrumented.Git{Next: &fakes.FakeGitHostClient{}, Metrics: m}
	policy := instrumented.Policy{Metrics: m, Next: &fakes.FakePolicyClient{EvaluateFunc: func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: []policyclient.ResolvedStep{
			{StepID: "review", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: true},
			{StepID: "lint", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseReview},
		}}, nil
	}}}
	w.runner = taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Audit: audit, IssueOpener: git, PRCreator: git,
			Executors: executor.NewRegistry(
				&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: audit},
				&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: audit},
			),
		},
		Store:    instrumented.Store{Next: w.store, Metrics: m},
		Launcher: launcher,
		Observer: m,
	})
	return w
}

func (w *world) run(t testing.TB, task domain.Task) taskrunner.Result {
	t.Helper()
	_, done, err := w.runner.Start(context.Background(), task)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := <-done
	if res.Err != nil {
		t.Fatalf("task %s failed: %v", task.ID, res.Err)
	}
	return res
}

// TestRunTask_MetricsMatchEvents is MT-1 end to end: after real tasks run
// through the runner, every event-derived counter equals the number of
// task_events rows of its type.
func TestRunTask_MetricsMatchEvents(t *testing.T) {
	reader := telemetrytest.New()
	w := newWorld(reader.Provider)
	for _, id := range []string{"task-1", "task-2"} {
		w.run(t, domain.Task{ID: id, TargetRepo: "acme/widgets"})
	}

	count := func(typ string) float64 {
		n := 0
		for _, id := range []string{"task-1", "task-2"} {
			for _, e := range w.store.Events(id) {
				if e.Type == typ {
					n++
				}
			}
		}
		return float64(n)
	}
	if count(domain.EventStepResolved) == 0 {
		t.Fatal("no step.resolved events; the test proves nothing")
	}
	for metricName, eventType := range map[string]string{
		"harness_tasks_accepted_total":   domain.EventTaskAccepted,
		"harness_tasks_finished_total":   domain.EventTaskFinished,
		"harness_task_rounds":            domain.EventTaskFinished,
		"harness_step_outcomes_total":    domain.EventStepResolved,
		"harness_step_duration_seconds":  domain.EventStepResolved,
		"harness_phase_duration_seconds": domain.EventPhaseFinished,
		"harness_pdp_decisions_total":    domain.EventPolicyDecided,
		"harness_rounds_total":           domain.EventRoundFinished,
		"harness_task_duration_seconds":  domain.EventTaskFinished,
	} {
		if got, want := reader.Value(t, metricName, nil), count(eventType); got != want {
			t.Errorf("%s = %v, want %v (%s events)", metricName, got, want, eventType)
		}
	}
	if got := reader.Value(t, "harness_tasks_finished_total", map[string]string{"outcome": "success", "target_repo": "acme/widgets"}); got != 2 {
		t.Errorf("successful tasks = %v, want 2", got)
	}
	if got := reader.Value(t, "harness_dependency_requests_total", map[string]string{"dependency": "launcher", "method": "Launch"}); got != 4 {
		t.Errorf("launcher Launch calls = %v, want 4 (2 tasks x 2 steps)", got)
	}
	if got := reader.Value(t, "harness_task_failures_total", nil); got != 0 {
		t.Errorf("harness_task_failures_total = %v for successful tasks", got)
	}
}
