// Package instrumented is the control-plane's metrics layer
// (platform-observability design §6): the instrument catalogue, the
// EventSink that derives task/round/phase/step metrics from committed
// task_events, the fleet gauges, and decorators that time every call to a
// dependency (Git host, PDP, secrets issuer, launcher, Postgres — MT-13).
//
// Metrics that describe a task's lifecycle are never emitted at call sites:
// Observe derives them from the same events the store commits, so the
// counters cannot drift from task_events (MT-1).
package instrumented

import (
	"context"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// Options bound the free-form labels (design §6, Q6).
type Options struct {
	// TargetRepos and StepIDs, when non-empty, are the label allow-lists
	// (TELEMETRY_TARGET_REPOS, TELEMETRY_STEP_IDS); other values become
	// "other". Empty means the first 64 distinct values, then "other".
	TargetRepos []string
	StepIDs     []string
}

// Metrics holds the control-plane's instruments. A nil *Metrics records
// nothing, so tests and callers that don't care can pass nil.
type Metrics struct {
	meter metric.Meter

	tasksAccepted   metric.Int64Counter
	tasksFinished   metric.Int64Counter
	taskDuration    metric.Float64Histogram
	taskActive      metric.Float64Histogram
	taskRounds      metric.Float64Histogram
	rounds          metric.Int64Counter
	policyOutcomes  metric.Int64Counter
	phaseDuration   metric.Float64Histogram
	stepDuration    metric.Float64Histogram
	stepOutcomes    metric.Int64Counter
	approvalWait    metric.Float64Histogram
	taskFailures    metric.Int64Counter
	approvals       metric.Int64Counter
	pdpDecisions    metric.Int64Counter
	pdpAllowedSteps metric.Int64Counter
	reconcile       metric.Int64Counter
	stepReconciled  metric.Int64Counter
	resumes         metric.Int64Counter
	depRequests     metric.Int64Counter
	depDuration     metric.Float64Histogram
	webhookEvents   metric.Int64Counter
	persistFailures metric.Int64Counter

	targetRepo *telemetry.Bounded
	stepID     *telemetry.Bounded
	decision   *telemetry.Bounded
}

// NewMetrics creates every control-plane instrument on m.
func NewMetrics(m metric.Meter, opts Options) *Metrics {
	tc := func(name, desc string, keys ...string) metric.Int64Counter {
		return telemetry.NewCounter(m, name, desc, "", keys...)
	}
	th := func(name, desc string, buckets []float64, keys ...string) metric.Float64Histogram {
		return telemetry.NewHistogram(m, name, desc, "s", buckets, keys...)
	}
	return &Metrics{
		meter:          m,
		tasksAccepted:  tc("harness_tasks_accepted_total", "Tasks accepted (MT-1).", "target_repo"),
		tasksFinished:  tc("harness_tasks_finished_total", "Tasks reaching success, escalated or failed; a retried task that fails again counts again (MT-1).", "outcome", "target_repo"),
		taskDuration:   th("harness_task_duration_seconds", "Task wall-clock time, acceptance to terminal (MT-2).", telemetry.TaskSecondsBuckets, "outcome"),
		taskActive:     th("harness_task_active_duration_seconds", "Task wall-clock time minus approval and operator waits (MT-2).", telemetry.TaskSecondsBuckets, "outcome"),
		taskRounds:     telemetry.NewHistogram(m, "harness_task_rounds", "Rounds run per finished task (MT-3).", "{round}", telemetry.RoundBuckets, "outcome", "target_repo"),
		rounds:         tc("harness_rounds_total", "Finished rounds by round number, clean or with blocking findings (MT-3, MT-4).", "round", "outcome"),
		policyOutcomes: tc("harness_policy_outcomes_total", "Governance outcomes ending a task; never failures (MT-8, MT-15).", "outcome"),
		phaseDuration:  th("harness_phase_duration_seconds", "Phase duration (MT-5).", telemetry.StepSecondsBuckets, "phase"),
		stepDuration:   th("harness_step_duration_seconds", "Step duration, launch to resolution (MT-6).", telemetry.StepSecondsBuckets, "step_type", "step_id", "outcome"),
		stepOutcomes:   tc("harness_step_outcomes_total", "Resolved steps by outcome (MT-6).", "step_type", "step_id", "outcome"),
		approvalWait:   th("harness_approval_wait_seconds", "Time from approval requested to granted or rejected (MT-7).", telemetry.TaskSecondsBuckets),
		taskFailures:   tc("harness_task_failures_total", "Tasks entering failed, by class (MT-8).", "error_class", "retryable", "dependency"),
		approvals:      tc("harness_approvals_total", "Approvals requested, granted and rejected (MT-15).", "outcome"),
		pdpDecisions:   tc("harness_pdp_decisions_total", "PDP decisions (MT-15).", "outcome"),
		pdpAllowedSteps: tc("harness_pdp_allowed_steps_total",
			"Steps resolved by allow decisions; launches should never exceed this (SLO-8)."),
		reconcile:       tc("harness_reconcile_total", "Startup reconciliation per task, by outcome (MT-10).", "outcome"),
		stepReconciled:  tc("harness_step_reconciled_total", "Launched steps re-resolved at startup (MT-10).", "outcome"),
		resumes:         tc("harness_resumes_total", "Rounds resumed, by reason (reconcile, approval, retry) (MT-10).", "outcome"),
		depRequests:     tc("harness_dependency_requests_total", "Calls to dependencies (MT-13).", "dependency", "method", "outcome", "code"),
		depDuration:     th("harness_dependency_request_duration_seconds", "Dependency call latency (MT-13).", telemetry.RequestSecondsBuckets, "dependency", "method", "outcome"),
		webhookEvents:   tc("harness_webhook_events_total", "Webhook deliveries by outcome; received is the sum (MT-14).", "outcome"),
		persistFailures: tc("harness_persistence_failures_total", "Writes or reads the control-plane could not complete against Postgres (ER-5).", "component"),

		targetRepo: telemetry.NewBounded(opts.TargetRepos, telemetry.DefaultBoundedMax),
		stepID:     telemetry.NewBounded(opts.StepIDs, telemetry.DefaultBoundedMax),
		decision:   telemetry.NewBounded([]string{"allow", "deny"}, 0),
	}
}

var _ taskrunner.RunnerMetrics = (*Metrics)(nil)

// Reconciled counts one startup-reconciliation outcome (taskrunner.Reconcile*).
func (m *Metrics) Reconciled(ctx context.Context, outcome string) {
	if m == nil {
		return
	}
	m.reconcile.Add(ctx, 1, telemetry.Attrs("outcome", outcome))
}

// PersistenceFailure counts a task failure the runner could not persist.
func (m *Metrics) PersistenceFailure(ctx context.Context) {
	m.persistenceFailure(ctx, "taskrunner")
}

func (m *Metrics) persistenceFailure(ctx context.Context, component string) {
	if m == nil {
		return
	}
	m.persistFailures.Add(ctx, 1, telemetry.Attrs("component", component))
}

// Webhook outcomes for WebhookEvent.
const (
	WebhookProcessed = "processed" // acted on: approval/rejection recorded, task started
	WebhookIgnored   = "ignored"   // valid, but nothing to do
	WebhookRejected  = "rejected"  // bad payload or signature
	WebhookFailed    = "failed"    // valid, but processing failed
)

// WebhookEvent counts one webhook delivery by outcome (MT-14).
func (m *Metrics) WebhookEvent(ctx context.Context, outcome string) {
	if m == nil {
		return
	}
	m.webhookEvents.Add(ctx, 1, telemetry.Attrs("outcome", outcome))
}

// FleetSource is the part of TaskStore the gauges read.
type FleetSource interface {
	FleetSnapshot(ctx context.Context, stuckAfter time.Duration) (domain.FleetSnapshot, error)
}

// gaugeTimeout bounds one gauge collection's database read.
const gaugeTimeout = 2 * time.Second

// RegisterFleetGauges registers harness_tasks{status}, harness_tasks_stuck,
// harness_approvals_waiting and harness_approvals_oldest_wait_seconds, all
// filled by one FleetSnapshot per collection (MT-7, MT-9). A failed read
// skips that collection and counts a persistence failure. Assumes one
// control-plane instance.
func (m *Metrics) RegisterFleetGauges(src FleetSource, stuckAfter time.Duration) {
	if m == nil {
		return
	}
	telemetry.DeclareGauge("harness_tasks", "status")
	telemetry.DeclareGauge("harness_tasks_stuck")
	telemetry.DeclareGauge("harness_approvals_waiting")
	telemetry.DeclareGauge("harness_approvals_oldest_wait_seconds")
	tasks, err1 := m.meter.Int64ObservableGauge("harness_tasks", metric.WithDescription("Tasks by current status (MT-9)."))
	stuck, err2 := m.meter.Int64ObservableGauge("harness_tasks_stuck",
		metric.WithDescription("Running tasks with no event for longer than STUCK_TASK_INTERVAL (MT-9, SLO-1)."))
	waiting, err3 := m.meter.Int64ObservableGauge("harness_approvals_waiting", metric.WithDescription("Tasks waiting for approval (MT-7)."))
	oldest, err4 := m.meter.Float64ObservableGauge("harness_approvals_oldest_wait_seconds",
		metric.WithDescription("Age of the oldest pending approval (MT-7, SLO-5)."), metric.WithUnit("s"))
	for _, err := range []error{err1, err2, err3, err4} {
		if err != nil {
			otel.Handle(err)
			return
		}
	}
	if _, err := m.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, gaugeTimeout)
		defer cancel()
		snap, err := src.FleetSnapshot(ctx, stuckAfter)
		if err != nil {
			m.persistenceFailure(ctx, "gauges")
			return nil // skip this collection; the error is counted, not exported
		}
		for _, s := range domain.TaskStatuses {
			o.ObserveInt64(tasks, int64(snap.ByStatus[s]), metric.WithAttributes(attrString("status", s)))
		}
		o.ObserveInt64(stuck, int64(snap.Stuck))
		o.ObserveInt64(waiting, int64(snap.WaitingApprovals))
		o.ObserveFloat64(oldest, snap.OldestApprovalWait.Seconds())
		return nil
	}, tasks, stuck, waiting, oldest); err != nil {
		otel.Handle(err)
	}
}

// RegisterDependencyUp registers harness_dependency_up{dependency}: 1 when
// up() reports the dependency reachable, else 0 (SLO-4). Each key of ups is
// a failure.Dep* name.
func (m *Metrics) RegisterDependencyUp(ups map[string]func() bool) {
	if m == nil {
		return
	}
	telemetry.DeclareGauge("harness_dependency_up", "dependency")
	if _, err := m.meter.Int64ObservableGauge("harness_dependency_up",
		metric.WithDescription("1 if the readiness prober last reached the dependency, else 0 (SLO-4)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for dep, up := range ups {
				v := int64(0)
				if up() {
					v = 1
				}
				o.Observe(v, metric.WithAttributes(attrString("dependency", dep)))
			}
			return nil
		})); err != nil {
		otel.Handle(err)
	}
}

// Observe derives metrics from events that were just committed (the store's
// event observer, design §6). It is a pure switch on the event type; types
// it doesn't know are ignored (the vocabulary is open).
func (m *Metrics) Observe(ctx context.Context, events []domain.TaskEvent) {
	if m == nil {
		return
	}
	for _, e := range events {
		m.observe(ctx, e)
	}
}

func (m *Metrics) observe(ctx context.Context, e domain.TaskEvent) {
	p := e.Payload
	switch e.Type {
	case domain.EventTaskAccepted:
		m.tasksAccepted.Add(ctx, 1, telemetry.Attrs("target_repo", m.targetRepo.Value(str(p, "target_repo"))))

	case domain.EventTaskFinished:
		outcome, repo := str(p, "outcome"), m.targetRepo.Value(str(p, "target_repo"))
		m.tasksFinished.Add(ctx, 1, telemetry.Attrs("outcome", outcome, "target_repo", repo))
		wall, wait := millis(p, "wall_ms"), millis(p, "wait_ms")
		m.taskDuration.Record(ctx, wall.Seconds(), telemetry.Attrs("outcome", outcome))
		m.taskActive.Record(ctx, (wall - wait).Seconds(), telemetry.Attrs("outcome", outcome))
		m.taskRounds.Record(ctx, num(p, "rounds"), telemetry.Attrs("outcome", outcome, "target_repo", repo))
		if kind := str(p, "reason_kind"); kind != "" {
			m.policyOutcomes.Add(ctx, 1, telemetry.Attrs("outcome", kind))
		}

	case domain.EventTaskFailed:
		m.tasksFinished.Add(ctx, 1, telemetry.Attrs("outcome", domain.TaskStatusFailed, "target_repo", m.targetRepo.Value(str(p, "target_repo"))))
		m.taskFailures.Add(ctx, 1, telemetry.Attrs("error_class", str(p, "error_class"),
			"retryable", strconv.FormatBool(boolean(p, "retryable")), "dependency", str(p, "dependency")))
		if _, terminal := p["wall_ms"]; terminal {
			m.taskDuration.Record(ctx, millis(p, "wall_ms").Seconds(), telemetry.Attrs("outcome", domain.TaskStatusFailed))
		}

	case domain.EventRoundFinished:
		// A round that ended the task by escalation isn't a clean/blocking
		// data point for convergence; waiting rounds never finish.
		switch str(p, "outcome") {
		case "continue", "stop-success":
			outcome := "clean"
			if boolean(p, "blocking_findings") {
				outcome = "blocking"
			}
			m.rounds.Add(ctx, 1, telemetry.Attrs("round", strconv.Itoa(e.Round), "outcome", outcome))
		}

	case domain.EventPhaseFinished:
		m.phaseDuration.Record(ctx, millis(p, "duration_ms").Seconds(), telemetry.Attrs("phase", str(p, "phase")))

	case domain.EventStepResolved:
		attrs := telemetry.Attrs("step_type", str(p, "step_type"), "step_id", m.stepID.Value(e.StepID), "outcome", str(p, "outcome"))
		m.stepDuration.Record(ctx, millis(p, "duration_ms").Seconds(), attrs)
		m.stepOutcomes.Add(ctx, 1, attrs)

	case domain.EventApprovalRequested:
		m.approvals.Add(ctx, 1, telemetry.Attrs("outcome", "requested"))
	case domain.EventApprovalGranted, domain.EventApprovalRejected:
		outcome := "granted"
		if e.Type == domain.EventApprovalRejected {
			outcome = "rejected"
		}
		m.approvals.Add(ctx, 1, telemetry.Attrs("outcome", outcome))
		m.approvalWait.Record(ctx, millis(p, "wait_ms").Seconds())

	case domain.EventPolicyDecided:
		decision := str(p, "decision")
		m.pdpDecisions.Add(ctx, 1, telemetry.Attrs("outcome", m.decision.Value(decision)))
		if decision == "allow" {
			m.pdpAllowedSteps.Add(ctx, int64(num(p, "resolved_steps")))
		}

	case domain.EventStepReconciled:
		outcome := "unresolved"
		if boolean(p, "resolved") {
			outcome = "resolved"
		}
		m.stepReconciled.Add(ctx, 1, telemetry.Attrs("outcome", outcome))

	case domain.EventRoundResumed:
		m.resumes.Add(ctx, 1, telemetry.Attrs("outcome", str(p, "reason")))
	}
}

// Payload readers. Events reach Observe straight from the writer (native Go
// types) but may also be decoded from JSON (float64), so numbers accept both.

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func boolean(p map[string]any, k string) bool {
	b, _ := p[k].(bool)
	return b
}

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

func millis(p map[string]any, k string) time.Duration {
	return time.Duration(num(p, k) * float64(time.Millisecond))
}
