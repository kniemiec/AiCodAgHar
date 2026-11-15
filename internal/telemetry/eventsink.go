package telemetry

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// Observe is the EventSink (design §6): a pure switch on event type that
// updates the task/phase/step instruments. The stores call it only after the
// transaction that wrote events committed, so these metrics agree with
// task_events (MT-1); a crash between commit and Observe loses one
// increment, which is accepted. Unknown event types are ignored.
func (m *ControlPlane) Observe(ctx context.Context, events []domain.TaskEvent) {
	if m == nil {
		return
	}
	for _, e := range events {
		m.observe(ctx, e)
	}
}

var _ domain.EventObserver = (*ControlPlane)(nil)

func (m *ControlPlane) observe(ctx context.Context, e domain.TaskEvent) {
	p := e.Payload
	switch e.Type {
	case domain.EventTaskAccepted:
		m.tasksAccepted.Add(ctx, 1, m.repo(p))

	case domain.EventTaskFinished:
		outcome := attribute.String("outcome", str(p, "outcome"))
		m.tasksFinished.Add(ctx, 1, outcome, m.repo(p))
		wall, wait := num(p, "wall_ms"), num(p, "wait_ms")
		m.taskDuration.Record(ctx, wall/1000, outcome)
		m.taskActive.Record(ctx, (wall-wait)/1000, outcome)
		m.taskRounds.Record(ctx, num(p, "rounds"), outcome, m.repo(p))
		if kind := str(p, "reason_kind"); kind != "" {
			m.policyOutcomes.Add(ctx, 1, attribute.String("outcome", kind))
		}

	case domain.EventTaskFailed:
		retryable, _ := p["retryable"].(bool)
		m.taskFailures.Add(ctx, 1, attribute.String("error_class", str(p, "error_class")),
			attribute.Bool("retryable", retryable), attribute.String("dependency", str(p, "dependency")))
		failed := attribute.String("outcome", "failed")
		m.tasksFinished.Add(ctx, 1, failed, m.repo(p))
		if _, terminal := p["wall_ms"]; terminal {
			m.taskDuration.Record(ctx, num(p, "wall_ms")/1000, failed)
		}

	case domain.EventRoundFinished:
		// Waiting-approval rounds never get round.finished; stop-escalate
		// rounds (policy deny, invalid config) didn't run steps to judge.
		switch str(p, "outcome") {
		case "continue", "stop-success":
			outcome := "clean"
			if b, _ := p["blocking_findings"].(bool); b {
				outcome = "blocking"
			}
			m.rounds.Add(ctx, 1, attribute.String("round", strconv.Itoa(e.Round)), attribute.String("outcome", outcome))
		}

	case domain.EventRoundResumed:
		m.resumes.Add(ctx, 1, attribute.String("outcome", str(p, "reason")))

	case domain.EventPolicyDecided:
		decision := str(p, "decision")
		m.pdpDecisions.Add(ctx, 1, attribute.String("outcome", decision))
		if decision == "allow" {
			m.pdpAllowedSteps.Add(ctx, int64(num(p, "resolved_steps")))
		}

	case domain.EventPhaseFinished:
		m.phaseDuration.Record(ctx, num(p, "duration_ms")/1000, attribute.String("phase", str(p, "phase")))

	case domain.EventStepResolved:
		attrs := []attribute.KeyValue{
			attribute.String("step_type", str(p, "step_type")),
			attribute.String("step_id", m.stepIDs.Value(e.StepID)),
			attribute.String("outcome", str(p, "outcome")),
		}
		m.stepOutcomes.Add(ctx, 1, attrs...)
		m.stepDuration.Record(ctx, num(p, "duration_ms")/1000, attrs...)

	case domain.EventStepReconciled:
		outcome := "unresolved"
		if b, _ := p["resolved"].(bool); b {
			outcome = "resolved"
		}
		m.stepReconciled.Add(ctx, 1, attribute.String("outcome", outcome))

	case domain.EventApprovalRequested:
		m.approvals.Add(ctx, 1, attribute.String("outcome", "requested"))

	case domain.EventApprovalGranted, domain.EventApprovalRejected:
		outcome := "granted"
		if e.Type == domain.EventApprovalRejected {
			outcome = "rejected"
		}
		m.approvals.Add(ctx, 1, attribute.String("outcome", outcome))
		if _, ok := p["wait_ms"]; ok {
			m.approvalWait.Record(ctx, num(p, "wait_ms")/1000)
		}
	}
}

func (m *ControlPlane) repo(p map[string]any) attribute.KeyValue {
	return attribute.String("target_repo", m.targetRepos.Value(str(p, "target_repo")))
}

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

// num reads a numeric payload field whether it was built in-process (int,
// int64) or decoded from JSON (float64).
func num(p map[string]any, k string) float64 {
	switch v := p[k].(type) {
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}
