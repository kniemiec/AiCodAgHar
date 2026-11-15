package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry/telemetrytest"
)

func ev(typ string, round int, payload map[string]any) domain.TaskEvent {
	return domain.TaskEvent{TaskID: "t", Round: round, Type: typ, Payload: payload}
}

// TestEventSink_MetricsMatchEvents is MT-1 at the sink level: a scripted
// event stream (two tasks: one approval-gated success over two rounds, one
// terminal failure; plus a policy deny) yields counters equal to the counts
// of the events behind them.
func TestEventSink_MetricsMatchEvents(t *testing.T) {
	reader := telemetrytest.New()
	m := telemetry.NewControlPlane(reader.Provider, telemetry.ControlPlaneOptions{})
	repo := map[string]any{"target_repo": "acme/widgets"}
	with := func(base map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	events := []domain.TaskEvent{
		ev(domain.EventTaskAccepted, 0, repo),
		ev(domain.EventRoundStarted, 1, nil),
		ev(domain.EventPolicyDecided, 1, map[string]any{"decision": "allow", "resolved_steps": 2}),
		ev(domain.EventPhaseStarted, 1, map[string]any{"phase": "review"}),
		{TaskID: "t", Round: 1, StepID: "review", Type: domain.EventStepResolved, Payload: map[string]any{"step_type": "agent-stage", "outcome": "completed", "duration_ms": int64(4000)}},
		{TaskID: "t", Round: 1, StepID: "lint", Type: domain.EventStepResolved, Payload: map[string]any{"step_type": "deterministic-step", "outcome": "completed", "duration_ms": int64(1000)}},
		ev(domain.EventPhaseFinished, 1, map[string]any{"phase": "review", "duration_ms": int64(4100)}),
		ev(domain.EventRoundFinished, 1, map[string]any{"outcome": "continue", "blocking_findings": true}),
		ev(domain.EventPolicyDecided, 2, map[string]any{"decision": "allow", "resolved_steps": 3}),
		ev(domain.EventApprovalRequested, 2, map[string]any{"issue_number": 9}),
		ev(domain.EventApprovalGranted, 2, map[string]any{"by": "alice", "wait_ms": int64(60_000)}),
		ev(domain.EventRoundResumed, 2, map[string]any{"reason": "approval"}),
		ev(domain.EventRoundFinished, 2, map[string]any{"outcome": "stop-success", "blocking_findings": false}),
		ev(domain.EventTaskFinished, 2, with(repo, "outcome", "success", "reason_kind", "", "rounds", 2, "wall_ms", int64(600_000), "wait_ms", int64(60_000))),

		ev(domain.EventTaskAccepted, 0, repo),
		ev(domain.EventPolicyDecided, 1, map[string]any{"decision": "deny", "resolved_steps": 0, "reason_kind": "policy_denied"}),
		ev(domain.EventRoundFinished, 1, map[string]any{"outcome": "stop-escalate", "blocking_findings": false}),
		ev(domain.EventTaskFinished, 1, with(repo, "outcome", "escalated", "reason_kind", "policy_denied", "rounds", 1, "wall_ms", int64(1000), "wait_ms", int64(0))),

		ev(domain.EventTaskAccepted, 0, repo),
		ev(domain.EventTaskFailed, 1, with(repo, "error_class", "dependency", "retryable", true, "dependency", "pdp")),
		ev(domain.EventTaskRetried, 1, nil),
		ev(domain.EventTaskFailed, 1, with(repo, "error_class", "agent", "retryable", false, "wall_ms", int64(90_000))),
		ev("some.future.event", 1, nil), // unknown types are ignored
	}
	m.Observe(context.Background(), events)

	count := func(typ string, pred func(map[string]any) bool) float64 {
		n := 0
		for _, e := range events {
			if e.Type == typ && (pred == nil || pred(e.Payload)) {
				n++
			}
		}
		return float64(n)
	}
	is := func(k string, v any) func(map[string]any) bool {
		return func(p map[string]any) bool { return p[k] == v }
	}

	checks := []struct {
		metric string
		match  map[string]string
		want   float64
	}{
		{"harness_tasks_accepted_total", nil, count(domain.EventTaskAccepted, nil)},
		{"harness_tasks_finished_total", map[string]string{"outcome": "success"}, count(domain.EventTaskFinished, is("outcome", "success"))},
		{"harness_tasks_finished_total", map[string]string{"outcome": "escalated"}, count(domain.EventTaskFinished, is("outcome", "escalated"))},
		{"harness_tasks_finished_total", map[string]string{"outcome": "failed"}, count(domain.EventTaskFailed, nil)},
		{"harness_task_failures_total", nil, count(domain.EventTaskFailed, nil)},
		{"harness_task_failures_total", map[string]string{"error_class": "dependency", "retryable": "true", "dependency": "pdp"}, 1},
		{"harness_task_duration_seconds", map[string]string{"outcome": "failed"}, 1}, // terminal failure only
		{"harness_task_active_duration_seconds", map[string]string{"outcome": "failed"}, 0},
		{"harness_task_rounds", nil, count(domain.EventTaskFinished, nil)},
		{"harness_policy_outcomes_total", map[string]string{"outcome": "policy_denied"}, 1},
		{"harness_rounds_total", nil, count(domain.EventRoundFinished, func(p map[string]any) bool { return p["outcome"] != "stop-escalate" })},
		{"harness_rounds_total", map[string]string{"round": "1", "outcome": "blocking"}, 1},
		{"harness_rounds_total", map[string]string{"round": "2", "outcome": "clean"}, 1},
		{"harness_phase_duration_seconds", nil, count(domain.EventPhaseFinished, nil)},
		{"harness_step_outcomes_total", nil, count(domain.EventStepResolved, nil)},
		{"harness_step_duration_seconds", map[string]string{"step_type": "agent-stage", "step_id": "review", "outcome": "completed"}, 1},
		{"harness_approvals_total", map[string]string{"outcome": "requested"}, count(domain.EventApprovalRequested, nil)},
		{"harness_approvals_total", map[string]string{"outcome": "granted"}, count(domain.EventApprovalGranted, nil)},
		{"harness_approval_wait_seconds", nil, count(domain.EventApprovalGranted, nil)},
		{"harness_pdp_decisions_total", nil, count(domain.EventPolicyDecided, nil)},
		{"harness_pdp_allowed_steps_total", nil, 5},
		{"harness_resumes_total", map[string]string{"outcome": "approval"}, count(domain.EventRoundResumed, nil)},
	}
	for _, c := range checks {
		if got := reader.Value(t, c.metric, c.match); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.metric, c.match, got, c.want)
		}
	}

	// Durations come from the payloads, not the clock.
	for _, p := range reader.Points(t, "harness_task_duration_seconds") {
		if p.Attrs["outcome"] == "success" && p.Sum != 600 {
			t.Errorf("success task duration sum = %v s, want 600", p.Sum)
		}
	}
	for _, p := range reader.Points(t, "harness_task_active_duration_seconds") {
		if p.Attrs["outcome"] == "success" && p.Sum != 540 {
			t.Errorf("success task active duration = %v s, want 540 (wall 600 - wait 60)", p.Sum)
		}
	}
}

func TestFleetGauges(t *testing.T) {
	reader := telemetrytest.New()
	m := telemetry.NewControlPlane(reader.Provider, telemetry.ControlPlaneOptions{})
	fail := false
	m.RegisterFleetGauges(func(context.Context) (domain.FleetSnapshot, error) {
		if fail {
			return domain.FleetSnapshot{}, errors.New("db down")
		}
		return domain.FleetSnapshot{
			TasksByStatus:      map[string]int64{domain.TaskStatusRunning: 3, domain.TaskStatusFailed: 1},
			Stuck:              1,
			ApprovalsWaiting:   2,
			OldestApprovalWait: 90 * time.Second,
		}, nil
	})

	for st, want := range map[string]float64{domain.TaskStatusRunning: 3, domain.TaskStatusFailed: 1, domain.TaskStatusStopSuccess: 0} {
		if got := reader.Value(t, "harness_tasks", map[string]string{"status": st}); got != want {
			t.Errorf("harness_tasks{status=%s} = %v, want %v", st, got, want)
		}
	}
	if n := len(reader.Points(t, "harness_tasks")); n != len(domain.TaskStatuses) {
		t.Errorf("harness_tasks has %d series, want one per status (%d)", n, len(domain.TaskStatuses))
	}
	for name, want := range map[string]float64{"harness_tasks_stuck": 1, "harness_approvals_waiting": 2, "harness_approvals_oldest_wait_seconds": 90} {
		if got := reader.Value(t, name, nil); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	fail = true
	if got := len(reader.Points(t, "harness_tasks")); got != 0 {
		t.Errorf("a failed snapshot still reported %d harness_tasks series", got)
	}
	if got := reader.Value(t, "harness_persistence_failures_total", map[string]string{"component": "fleet_snapshot"}); got < 1 {
		t.Errorf("failed snapshot not counted in harness_persistence_failures_total")
	}
}

func TestDependencyUp(t *testing.T) {
	reader := telemetrytest.New()
	m := telemetry.NewControlPlane(reader.Provider, telemetry.ControlPlaneOptions{})
	pdpUp := true
	m.RegisterDependencyUp(
		telemetry.DependencyProbe{Dependency: "pdp", Up: func() bool { return pdpUp }},
		telemetry.DependencyProbe{Dependency: "secrets_issuer", Up: func() bool { return false }},
	)
	if reader.Value(t, "harness_dependency_up", map[string]string{"dependency": "pdp"}) != 1 ||
		reader.Value(t, "harness_dependency_up", map[string]string{"dependency": "secrets_issuer"}) != 0 {
		t.Fatalf("dependency_up = %+v", reader.Points(t, "harness_dependency_up"))
	}
	pdpUp = false
	if reader.Value(t, "harness_dependency_up", map[string]string{"dependency": "pdp"}) != 0 {
		t.Fatal("pdp still up after its probe failed")
	}
}

func TestBoundedLabelsInSink(t *testing.T) {
	reader := telemetrytest.New()
	m := telemetry.NewControlPlane(reader.Provider, telemetry.ControlPlaneOptions{
		TargetRepos: telemetry.NewBounded([]string{"acme/widgets"}, telemetry.DefaultBoundedMax),
	})
	m.Observe(context.Background(), []domain.TaskEvent{
		ev(domain.EventTaskAccepted, 0, map[string]any{"target_repo": "acme/widgets"}),
		ev(domain.EventTaskAccepted, 0, map[string]any{"target_repo": "someone/else"}),
	})
	if reader.Value(t, "harness_tasks_accepted_total", map[string]string{"target_repo": "acme/widgets"}) != 1 ||
		reader.Value(t, "harness_tasks_accepted_total", map[string]string{"target_repo": telemetry.Other}) != 1 {
		t.Fatalf("target_repo not bounded: %+v", reader.Points(t, "harness_tasks_accepted_total"))
	}
}

func TestNilControlPlaneIsNoop(t *testing.T) {
	var m *telemetry.ControlPlane
	m.Observe(context.Background(), []domain.TaskEvent{ev(domain.EventTaskAccepted, 0, nil)})
	m.DependencyCall(context.Background(), "pdp", "Evaluate", "", time.Second, nil)
	m.WebhookEvent(context.Background(), telemetry.WebhookIgnored)
	m.Reconciled(context.Background(), "noop")
	m.PersistenceFailure(context.Background(), "x")
	m.RegisterFleetGauges(nil)
	m.RegisterDependencyUp()
}
