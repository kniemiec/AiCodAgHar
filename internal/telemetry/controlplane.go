package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// ControlPlane is the control-plane's metric catalogue (design §6). A nil
// *ControlPlane records nothing, so it can be left unwired in tests.
//
// Task-, phase- and step-level metrics come only from Observe (the
// EventSink), fed with committed task_events; everything with no event
// behind it (dependency calls, webhook intake, reconciliation, gauges) is
// recorded directly.
type ControlPlane struct {
	reg         *registry
	targetRepos *Bounded
	stepIDs     *Bounded

	tasksAccepted   *Counter
	tasksFinished   *Counter
	taskDuration    *Histogram
	taskActive      *Histogram
	taskRounds      *Histogram
	rounds          *Counter
	policyOutcomes  *Counter
	phaseDuration   *Histogram
	stepDuration    *Histogram
	stepOutcomes    *Counter
	approvalWait    *Histogram
	taskFailures    *Counter
	approvals       *Counter
	pdpDecisions    *Counter
	pdpAllowedSteps *Counter
	reconcile       *Counter
	stepReconciled  *Counter
	resumes         *Counter
	depRequests     *Counter
	depDuration     *Histogram
	webhookEvents   *Counter
	persistFailures *Counter

	tasks          metric.Int64ObservableGauge
	tasksStuck     metric.Int64ObservableGauge
	approvalsWait  metric.Int64ObservableGauge
	approvalOldest metric.Float64ObservableGauge
	dependencyUp   metric.Int64ObservableGauge
}

// ControlPlaneOptions bound the free-form labels (TELEMETRY_TARGET_REPOS,
// TELEMETRY_STEP_IDS). Nil means first-seen up to DefaultBoundedMax.
type ControlPlaneOptions struct {
	TargetRepos *Bounded
	StepIDs     *Bounded
}

// NewControlPlane builds the catalogue on mp.
func NewControlPlane(mp metric.MeterProvider, opts ControlPlaneOptions) *ControlPlane {
	if opts.TargetRepos == nil {
		opts.TargetRepos = NewBounded(nil, DefaultBoundedMax)
	}
	if opts.StepIDs == nil {
		opts.StepIDs = NewBounded(nil, DefaultBoundedMax)
	}
	r := newRegistry(mp)
	return &ControlPlane{
		reg: r, targetRepos: opts.TargetRepos, stepIDs: opts.StepIDs,

		tasksAccepted:   r.counter("harness_tasks_accepted_total", "Tasks accepted (task.accepted).", "target_repo"),
		tasksFinished:   r.counter("harness_tasks_finished_total", "Task terminal outcomes; failed counts every entry into failed.", "outcome", "target_repo"),
		taskDuration:    r.histogram("harness_task_duration_seconds", "Task wall time, accepted to terminal.", "s", taskBuckets, "outcome"),
		taskActive:      r.histogram("harness_task_active_duration_seconds", "Task wall time minus approval and retry waits.", "s", taskBuckets, "outcome"),
		taskRounds:      r.histogram("harness_task_rounds", "Rounds a finished task ran.", "", roundBuckets, "outcome", "target_repo"),
		rounds:          r.counter("harness_rounds_total", "Rounds that ran to completion, by whether blocking findings remained.", "round", "outcome"),
		policyOutcomes:  r.counter("harness_policy_outcomes_total", "Governance stop-escalate outcomes (never failures).", "outcome"),
		phaseDuration:   r.histogram("harness_phase_duration_seconds", "Phase duration.", "s", stepBuckets, "phase"),
		stepDuration:    r.histogram("harness_step_duration_seconds", "Step duration, launched to resolved.", "s", stepBuckets, "step_type", "step_id", "outcome"),
		stepOutcomes:    r.counter("harness_step_outcomes_total", "Resolved steps by outcome.", "step_type", "step_id", "outcome"),
		approvalWait:    r.histogram("harness_approval_wait_seconds", "Approval requested to granted/rejected.", "s", approvalBuckets),
		taskFailures:    r.counter("harness_task_failures_total", "Entries into status=failed.", "error_class", "retryable", "dependency"),
		approvals:       r.counter("harness_approvals_total", "Approval lifecycle events.", "outcome"),
		pdpDecisions:    r.counter("harness_pdp_decisions_total", "PDP decisions by value.", "outcome"),
		pdpAllowedSteps: r.counter("harness_pdp_allowed_steps_total", "Steps resolved by allow decisions."),
		reconcile:       r.counter("harness_reconcile_total", "Startup reconciliation results per task.", "outcome"),
		stepReconciled:  r.counter("harness_step_reconciled_total", "Launched steps re-queried at startup.", "outcome"),
		resumes:         r.counter("harness_resumes_total", "Rounds re-entered (reconcile, approval, retry).", "outcome"),
		depRequests:     r.counter("harness_dependency_requests_total", "Calls to dependencies.", "dependency", "method", "outcome", "code"),
		depDuration:     r.histogram("harness_dependency_request_duration_seconds", "Dependency call latency.", "s", depBuckets, "dependency", "method"),
		webhookEvents:   r.counter("harness_webhook_events_total", "Forgejo webhook deliveries by outcome.", "outcome"),
		persistFailures: r.counter("harness_persistence_failures_total", "Writes or polls against Postgres that failed outside a task's own error path.", "component"),

		tasks:          r.gauge("harness_tasks", "Tasks per status.", "status"),
		tasksStuck:     r.gauge("harness_tasks_stuck", "Running tasks with no event for longer than STUCK_TASK_INTERVAL."),
		approvalsWait:  r.gauge("harness_approvals_waiting", "Tasks waiting for approval."),
		approvalOldest: r.floatGauge("harness_approvals_oldest_wait_seconds", "Age of the oldest pending approval.", "s"),
		dependencyUp:   r.gauge("harness_dependency_up", "1 if the dependency answered its last probe.", "dependency"),
	}
}

// Catalogue lists the instruments NewControlPlane defines.
func (m *ControlPlane) Catalogue() []Def { return m.reg.defs }

// fleetTimeout bounds one gauge collection's database poll.
const fleetTimeout = 2 * time.Second

// RegisterFleetGauges polls snapshot on every collection for the task and
// approval gauges (MT-7, MT-9). A failed poll skips that collection and
// counts a persistence failure. Assumes one control-plane instance.
func (m *ControlPlane) RegisterFleetGauges(snapshot func(context.Context) (domain.FleetSnapshot, error)) {
	if m == nil {
		return
	}
	_, err := m.reg.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, fleetTimeout)
		defer cancel()
		snap, err := snapshot(ctx)
		if err != nil {
			m.PersistenceFailure(ctx, "fleet_snapshot")
			return nil
		}
		for _, st := range domain.TaskStatuses {
			o.ObserveInt64(m.tasks, snap.TasksByStatus[st], metric.WithAttributes(attribute.String("status", st)))
		}
		o.ObserveInt64(m.tasksStuck, snap.Stuck)
		o.ObserveInt64(m.approvalsWait, snap.ApprovalsWaiting)
		o.ObserveFloat64(m.approvalOldest, snap.OldestApprovalWait.Seconds())
		return nil
	}, m.tasks, m.tasksStuck, m.approvalsWait, m.approvalOldest)
	if err != nil {
		otel.Handle(err)
	}
}

// DependencyProbe reports one dependency's last readiness probe.
type DependencyProbe struct {
	Dependency string
	Up         func() bool
}

// RegisterDependencyUp backs harness_dependency_up (SLO-4) with the
// readiness probers' latest results.
func (m *ControlPlane) RegisterDependencyUp(probes ...DependencyProbe) {
	if m == nil {
		return
	}
	_, err := m.reg.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, p := range probes {
			var v int64
			if p.Up() {
				v = 1
			}
			o.ObserveInt64(m.dependencyUp, v, metric.WithAttributes(attribute.String("dependency", p.Dependency)))
		}
		return nil
	}, m.dependencyUp)
	if err != nil {
		otel.Handle(err)
	}
}

// DependencyCall records one call to a dependency (MT-13). code is the gRPC
// code for the launcher and "" elsewhere.
func (m *ControlPlane) DependencyCall(ctx context.Context, dependency, method, code string, d time.Duration, err error) {
	if m == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	dep, meth := attribute.String("dependency", dependency), attribute.String("method", method)
	m.depRequests.Add(ctx, 1, dep, meth, attribute.String("outcome", outcome), attribute.String("code", code))
	m.depDuration.Record(ctx, d.Seconds(), dep, meth)
}

// Webhook outcomes for WebhookEvent (MT-14); received = their sum.
const (
	WebhookProcessed = "processed" // an approval/rejection recorded or a task started
	WebhookIgnored   = "ignored"   // valid, but nothing to do
	WebhookRejected  = "rejected"  // bad payload, signature, or unauthorized sender
	WebhookFailed    = "failed"    // our own processing failed
)

// WebhookEvent counts one webhook delivery.
func (m *ControlPlane) WebhookEvent(ctx context.Context, outcome string) {
	if m == nil {
		return
	}
	m.webhookEvents.Add(ctx, 1, attribute.String("outcome", outcome))
}

// Reconciled counts one task's startup-reconciliation result (MT-10):
// resumed, failed (unreconcilable launched step), noop, error.
func (m *ControlPlane) Reconciled(ctx context.Context, outcome string) {
	if m == nil {
		return
	}
	m.reconcile.Add(ctx, 1, attribute.String("outcome", outcome))
}

// PersistenceFailure counts a Postgres write or poll that failed outside a
// task's own error path (ER-5), e.g. the failure record itself.
func (m *ControlPlane) PersistenceFailure(ctx context.Context, component string) {
	if m == nil {
		return
	}
	m.persistFailures.Add(ctx, 1, attribute.String("component", component))
}
