package instrumented_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/instrumented"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// harness is a runner over fakes, with every dependency instrumented and
// the store's events feeding the sink — main.go's wiring, in memory.
type harness struct {
	metrics  *instrumented.Metrics
	reader   *sdkmetric.ManualReader
	store    *fakes.FakeTaskStore
	launcher *fakes.FakeContainerLauncher
	policy   *fakes.FakePolicyClient
	secrets  *fakes.FakeSecretsClient
	runner   *taskrunner.Runner
}

func newMetrics(t *testing.T) (*instrumented.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(telemetry.AllowedKeysView()))
	return instrumented.NewMetrics(mp.Meter("test"), instrumented.Options{}), reader
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	m, r := newMetrics(t)
	return newHarnessWith(m, r)
}

func newHarnessWith(metrics *instrumented.Metrics, reader *sdkmetric.ManualReader) *harness {
	h := &harness{
		store:    &fakes.FakeTaskStore{},
		launcher: &fakes.FakeContainerLauncher{},
		secrets:  &fakes.FakeSecretsClient{},
		policy:   &fakes.FakePolicyClient{},
		metrics:  metrics,
		reader:   reader,
	}
	h.store.Observer = h.metrics.Observe
	launcher := instrumented.Launcher{Next: h.launcher, M: h.metrics}
	secrets := instrumented.Secrets{Next: h.secrets, M: h.metrics}
	auditW := instrumented.Audit{Next: &fakes.FakeAuditWriter{}, M: h.metrics}
	git := instrumented.Git{Next: &fakes.FakeGitHostClient{}, M: h.metrics}
	h.runner = taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy:   instrumented.Policy{Next: h.policy, M: h.metrics},
			Approval: &fakes.FakeApprovalVerifier{}, Audit: auditW,
			Executors: executor.NewRegistry(
				&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditW},
				&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditW},
			),
			IssueOpener: git, PRCreator: git,
		},
		Store:    instrumented.Store{Next: h.store, M: h.metrics},
		Launcher: launcher,
		Metrics:  h.metrics,
	})
	return h
}

func (h *harness) run(t *testing.T, id, repo string) taskrunner.Result {
	t.Helper()
	_, done, err := h.runner.Start(context.Background(), domain.Task{ID: id, TargetRepo: repo, Description: "do the thing"})
	if err != nil {
		t.Fatalf("Start %s: %v", id, err)
	}
	return <-done
}

// policyFor scripts the PDP per task: deny for ids in deny, else allow one
// review step.
func policyFor(deny ...string) func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
	return func(_ context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		if slices.Contains(deny, req.TaskID) {
			return policyclient.PolicyDecision{Decision: "deny", Reason: "nope"}, nil
		}
		return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: []policyclient.ResolvedStep{
			{StepID: "review", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview},
		}}, nil
	}
}

// MT-1: every task counter equals the count of the events it derives from.
func TestMetrics_EventCountsMatchCounters(t *testing.T) {
	h := newHarness(t)
	h.policy.EvaluateFunc = policyFor("task-denied")
	h.launcher.LaunchFunc = func(_ context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
		res := launcherclient.RunResult{InvocationID: req.InvocationID, Status: "completed", Outcome: launcherclient.OutcomeCompleted,
			QueueWait: 3 * time.Second, Boot: time.Second, GuestRun: 2 * time.Second, EffectiveTimeout: 1800 * time.Second}
		if req.TaskID == "task-timeout" {
			res.Status, res.Outcome = "timed_out", launcherclient.OutcomeTimeout
			return res, &launcherclient.LaunchError{Result: res, Outcome: res.Outcome, Err: status.Error(codes.Internal, "timed out")}
		}
		return res, nil
	}

	ids := []string{"task-ok-1", "task-ok-2", "task-denied", "task-timeout"}
	for _, id := range ids {
		h.run(t, id, "acme/widgets")
	}

	count := map[string]int{}
	var finished = map[string]int{}
	for _, id := range ids {
		for _, e := range h.store.Events(id) {
			count[e.Type]++
			if e.Type == domain.EventTaskFinished {
				finished[e.Payload["outcome"].(string)]++
			}
			if e.Type == domain.EventStepResolved && id == "task-ok-1" {
				p := e.Payload
				if p["launcher_outcome"] != launcherclient.OutcomeCompleted || p["queue_wait_ms"] != int64(3000) ||
					p["step_type"] != executor.StepTypeAgentStage || p["effective_timeout_s"] != int64(1800) {
					t.Errorf("step.resolved payload lacks launcher detail: %v", p)
				}
			}
			if e.Type == domain.EventStepResolved && id == "task-timeout" && e.Payload["launcher_outcome"] != launcherclient.OutcomeTimeout {
				t.Errorf("failed step.resolved lacks launcher outcome: %v", e.Payload)
			}
		}
	}
	m := collect(t, h.reader)

	checks := []struct {
		name  string
		attrs map[string]string
		want  int
	}{
		{"harness_tasks_accepted_total", nil, count[domain.EventTaskAccepted]},
		{"harness_tasks_finished_total", map[string]string{"outcome": "success"}, finished["success"]},
		{"harness_tasks_finished_total", map[string]string{"outcome": "escalated"}, finished["escalated"]},
		{"harness_tasks_finished_total", map[string]string{"outcome": "failed"}, count[domain.EventTaskFailed]},
		{"harness_task_failures_total", map[string]string{"error_class": "agent", "retryable": "false"}, 1},
		{"harness_policy_outcomes_total", map[string]string{"outcome": domain.ReasonPolicyDenied}, 1},
		{"harness_step_outcomes_total", nil, count[domain.EventStepResolved]},
		{"harness_pdp_decisions_total", nil, count[domain.EventPolicyDecided]},
		{"harness_pdp_allowed_steps_total", nil, 3},
		{"harness_rounds_total", map[string]string{"round": "1", "outcome": "clean"}, 2},
		{"harness_dependency_requests_total", map[string]string{"dependency": "pdp"}, len(h.policy.Calls())},
		{"harness_dependency_requests_total", map[string]string{"dependency": "launcher", "method": "launch"}, len(h.launcher.Launches())},
	}
	for _, c := range checks {
		if got := sum(m, c.name, c.attrs); got != int64(c.want) {
			t.Errorf("%s%v = %d, want %d", c.name, c.attrs, got, c.want)
		}
	}
	if finished["success"] != 2 || finished["escalated"] != 1 || count[domain.EventTaskFailed] != 1 {
		t.Fatalf("scenario didn't produce 2 success / 1 escalated / 1 failed: %v %v", finished, count)
	}
	if n := histCount(m, "harness_task_duration_seconds", map[string]string{"outcome": "failed"}); n != 1 {
		t.Errorf("terminal failure not observed in harness_task_duration_seconds: count %d", n)
	}
	if n := histCount(m, "harness_task_active_duration_seconds", map[string]string{"outcome": "failed"}); n != 0 {
		t.Errorf("active duration observed for a failed task: count %d", n)
	}
}

// ER-3 / MT-8: governance outcomes never touch the failure counter.
func TestMetrics_PolicyOutcomesAreNotFailures(t *testing.T) {
	h := newHarness(t)
	h.policy.EvaluateFunc = policyFor("task-denied")
	h.run(t, "task-denied", "acme/widgets")
	m := collect(t, h.reader)
	if n := sum(m, "harness_task_failures_total", nil); n != 0 {
		t.Errorf("harness_task_failures_total = %d after a PDP deny, want 0", n)
	}
}

func TestMetrics_ScriptedEvents(t *testing.T) {
	metrics, reader := newMetrics(t)
	metrics.Observe(context.Background(), []domain.TaskEvent{
		{Type: domain.EventRoundFinished, Round: 2, Payload: map[string]any{"outcome": "continue", "blocking_findings": true}},
		{Type: domain.EventRoundFinished, Round: 3, Payload: map[string]any{"outcome": "stop-escalate", "blocking_findings": true}},
		{Type: domain.EventApprovalRequested},
		{Type: domain.EventApprovalGranted, Payload: map[string]any{"wait_ms": int64(90_000)}},
		{Type: domain.EventPhaseFinished, Payload: map[string]any{"phase": "review", "duration_ms": float64(1500)}}, // JSON-decoded shape
		{Type: domain.EventRoundResumed, Payload: map[string]any{"reason": domain.RoundResumeApproval}},
		{Type: domain.EventStepReconciled, Payload: map[string]any{"resolved": false}},
		{Type: "some.future.event"},
	})
	m := collect(t, reader)
	for _, c := range []struct {
		name  string
		attrs map[string]string
		want  int64
	}{
		{"harness_rounds_total", map[string]string{"round": "2", "outcome": "blocking"}, 1},
		{"harness_rounds_total", map[string]string{"round": "3"}, 0},
		{"harness_approvals_total", map[string]string{"outcome": "requested"}, 1},
		{"harness_approvals_total", map[string]string{"outcome": "granted"}, 1},
		{"harness_resumes_total", map[string]string{"outcome": "approval"}, 1},
		{"harness_step_reconciled_total", map[string]string{"outcome": "unresolved"}, 1},
	} {
		if got := sum(m, c.name, c.attrs); got != c.want {
			t.Errorf("%s%v = %d, want %d", c.name, c.attrs, got, c.want)
		}
	}
	if s := histSum(m, "harness_approval_wait_seconds"); s != 90 {
		t.Errorf("approval wait sum = %v, want 90", s)
	}
	if s := histSum(m, "harness_phase_duration_seconds"); s != 1.5 {
		t.Errorf("phase duration sum = %v, want 1.5", s)
	}
}

func TestMetrics_FleetGauges(t *testing.T) {
	metrics, reader := newMetrics(t)
	store := &fakes.FakeTaskStore{}
	ctx := context.Background()
	for _, id := range []string{"t-run", "t-wait"} {
		if err := store.CreateTask(ctx, domain.Task{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	issue := 7
	if err := store.UpdateTaskState(ctx, domain.TaskState{TaskID: "t-wait", Status: domain.TaskStatusWaitingApproval, ApprovalIssueNumber: &issue},
		domain.TaskEvent{TaskID: "t-wait", Type: domain.EventApprovalRequested}); err != nil {
		t.Fatal(err)
	}
	metrics.RegisterFleetGauges(store, 0) // every running task counts as stuck
	m := collect(t, reader)
	if v := gauge(m, "harness_tasks", map[string]string{"status": "running"}); v != 1 {
		t.Errorf("harness_tasks{running} = %v, want 1", v)
	}
	if v := gauge(m, "harness_tasks", map[string]string{"status": "failed"}); v != 0 {
		t.Errorf("harness_tasks{failed} = %v, want 0 (reported, not absent)", v)
	}
	if v := gauge(m, "harness_tasks_stuck", nil); v != 1 {
		t.Errorf("harness_tasks_stuck = %v, want 1", v)
	}
	if v := gauge(m, "harness_approvals_waiting", nil); v != 1 {
		t.Errorf("harness_approvals_waiting = %v, want 1", v)
	}
	if v := gauge(m, "harness_approvals_oldest_wait_seconds", nil); v <= 0 {
		t.Errorf("harness_approvals_oldest_wait_seconds = %v, want > 0", v)
	}
}

type brokenFleet struct{}

func (brokenFleet) FleetSnapshot(context.Context, time.Duration) (domain.FleetSnapshot, error) {
	return domain.FleetSnapshot{}, errors.New("db down")
}

func TestMetrics_FleetGaugeReadFailureIsCounted(t *testing.T) {
	metrics, reader := newMetrics(t)
	metrics.RegisterFleetGauges(brokenFleet{}, time.Minute)
	m := collect(t, reader)
	if n := sum(m, "harness_persistence_failures_total", map[string]string{"component": "gauges"}); n != 1 {
		t.Errorf("harness_persistence_failures_total{gauges} = %d, want 1", n)
	}
}

func TestMetrics_DependencyUp(t *testing.T) {
	metrics, reader := newMetrics(t)
	metrics.RegisterDependencyUp(map[string]func() bool{"pdp": func() bool { return true }, "secrets_issuer": func() bool { return false }})
	m := collect(t, reader)
	if v := gauge(m, "harness_dependency_up", map[string]string{"dependency": "pdp"}); v != 1 {
		t.Errorf("pdp up = %v", v)
	}
	if v := gauge(m, "harness_dependency_up", map[string]string{"dependency": "secrets_issuer"}); v != 0 {
		t.Errorf("secrets_issuer up = %v", v)
	}
}

func TestLauncherDecorator_RecordsCodeAndOutcome(t *testing.T) {
	metrics, reader := newMetrics(t)
	l := instrumented.Launcher{M: metrics, Next: &fakes.FakeContainerLauncher{
		LaunchFunc: func(context.Context, launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
			return launcherclient.RunResult{}, status.Error(codes.Unavailable, "down")
		},
	}}
	_, _ = l.Launch(context.Background(), launcherclient.LaunchRequest{})
	_, _ = l.ExtractLogs(context.Background(), "inv")
	m := collect(t, reader)
	if n := sum(m, "harness_dependency_requests_total", map[string]string{"dependency": "launcher", "method": "launch", "outcome": "error", "code": "Unavailable"}); n != 1 {
		t.Errorf("failed launch not recorded with its code: %d", n)
	}
	if n := sum(m, "harness_dependency_requests_total", map[string]string{"method": "extract_logs", "outcome": "ok", "code": "OK"}); n != 1 {
		t.Errorf("ok extract_logs not recorded: %d", n)
	}
}

// CC-2: every instrument in the catalogue uses allowed keys only, and none
// is keyed by a per-task, per-invocation or per-trace identifier.
func TestCatalogue_NoUnboundedKeys(t *testing.T) {
	metrics, _ := newMetrics(t)
	metrics.RegisterFleetGauges(brokenFleet{}, time.Minute)
	metrics.RegisterDependencyUp(nil)
	names := map[string]bool{}
	for _, in := range telemetry.Catalogue() {
		names[in.Name] = true
		for _, k := range in.Keys {
			if !slices.Contains(telemetry.AllowedKeys, k) || k == "task_id" || k == "invocation_id" || k == "trace_id" {
				t.Errorf("%s uses key %q", in.Name, k)
			}
		}
	}
	for _, want := range []string{
		"harness_tasks_accepted_total", "harness_tasks_finished_total", "harness_task_duration_seconds",
		"harness_task_active_duration_seconds", "harness_task_rounds", "harness_rounds_total", "harness_policy_outcomes_total",
		"harness_phase_duration_seconds", "harness_step_duration_seconds", "harness_step_outcomes_total",
		"harness_approval_wait_seconds", "harness_task_failures_total", "harness_approvals_total",
		"harness_pdp_decisions_total", "harness_pdp_allowed_steps_total", "harness_reconcile_total",
		"harness_step_reconciled_total", "harness_resumes_total", "harness_tasks", "harness_tasks_stuck",
		"harness_approvals_waiting", "harness_approvals_oldest_wait_seconds", "harness_dependency_requests_total",
		"harness_dependency_request_duration_seconds", "harness_dependency_up", "harness_webhook_events_total",
		"harness_persistence_failures_total",
	} {
		if !names[want] {
			t.Errorf("catalogue lacks %s", want)
		}
	}
}

// --- metric readers ---

func collect(t *testing.T, r *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

func matches(set attribute.Set, want map[string]string) bool {
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
			return false
		}
	}
	return true
}

func sum(ms []metricdata.Metrics, name string, want map[string]string) int64 {
	var total int64
	for _, m := range ms {
		if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == name {
			for _, dp := range s.DataPoints {
				if matches(dp.Attributes, want) {
					total += dp.Value
				}
			}
		}
	}
	return total
}

func histCount(ms []metricdata.Metrics, name string, want map[string]string) uint64 {
	var n uint64
	for _, m := range ms {
		if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
			for _, dp := range h.DataPoints {
				if matches(dp.Attributes, want) {
					n += dp.Count
				}
			}
		}
	}
	return n
}

func histSum(ms []metricdata.Metrics, name string) float64 {
	var s float64
	for _, m := range ms {
		if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
			for _, dp := range h.DataPoints {
				s += dp.Sum
			}
		}
	}
	return s
}

func gauge(ms []metricdata.Metrics, name string, want map[string]string) float64 {
	for _, m := range ms {
		if m.Name != name {
			continue
		}
		switch g := m.Data.(type) {
		case metricdata.Gauge[int64]:
			for _, dp := range g.DataPoints {
				if matches(dp.Attributes, want) {
					return float64(dp.Value)
				}
			}
		case metricdata.Gauge[float64]:
			for _, dp := range g.DataPoints {
				if matches(dp.Attributes, want) {
					return dp.Value
				}
			}
		}
	}
	return -1
}
