package main

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// launcherMetrics is the launcher's slice of the metrics catalogue
// (platform-observability design §6: MT-11, MT-12, MT-16). A nil
// *launcherMetrics records nothing.
type launcherMetrics struct {
	invocations metric.Int64Counter
	queueWait   metric.Float64Histogram
	boot        metric.Float64Histogram
	guestRun    metric.Float64Histogram
	vmSeconds   metric.Float64Counter
	redactions  metric.Int64Counter

	// stepType bounds the step_type label to the two known types.
	stepType *telemetry.Bounded
}

// newLauncherMetrics creates the instruments on m. srv's queued-Launch count
// backs the launcher_queue_depth gauge.
func newLauncherMetrics(m metric.Meter, srv *launcherServer) *launcherMetrics {
	lm := &launcherMetrics{
		invocations: telemetry.NewCounter(m, "launcher_invocations_total",
			"Launch invocations by outcome (LA-1).", "{invocation}", "outcome", "step_type"),
		queueWait: telemetry.NewHistogram(m, "launcher_queue_wait_seconds",
			"Time a Launch waited for the VM slot.", "s", telemetry.LauncherSecondsBuckets, "step_type"),
		boot: telemetry.NewHistogram(m, "launcher_boot_seconds",
			"Jail setup through InstanceStart.", "s", telemetry.LauncherSecondsBuckets, "step_type"),
		guestRun: telemetry.NewHistogram(m, "launcher_guest_run_seconds",
			"InstanceStart through guest completion.", "s", telemetry.LauncherSecondsBuckets, "step_type"),
		vmSeconds: telemetry.NewFloatCounter(m, "launcher_vm_seconds_total",
			"VM time consumed (boot plus run), MT-16.", "s", "step_type"),
		redactions: telemetry.NewCounter(m, "launcher_redactions_total",
			"Secret values redacted from guest output (MT-12).", "{redaction}", "step_type"),
		stepType: telemetry.NewBounded([]string{stepTypeAgentStage, stepTypeDeterministicStep}, 0),
	}
	telemetry.DeclareGauge("launcher_queue_depth")
	if _, err := m.Int64ObservableGauge("launcher_queue_depth",
		metric.WithDescription("Launch calls waiting for the VM slot."), metric.WithUnit("{invocation}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(srv.waiters.Load())
			return nil
		})); err != nil {
		otel.Handle(err)
	}
	return lm
}

// observe records one finished Launch.
func (lm *launcherMetrics) observe(ctx context.Context, stepType string, outcome launcherv1.LaunchOutcome, queueWait, boot, run time.Duration, redactions int) {
	if lm == nil {
		return
	}
	st := lm.stepType.Value(stepType)
	attrs := telemetry.Attrs("step_type", st)
	lm.invocations.Add(ctx, 1, telemetry.Attrs("outcome", outcomeLabel(outcome), "step_type", st))
	lm.queueWait.Record(ctx, queueWait.Seconds(), attrs)
	if boot > 0 {
		lm.boot.Record(ctx, boot.Seconds(), attrs)
	}
	if run > 0 {
		lm.guestRun.Record(ctx, run.Seconds(), attrs)
	}
	if vm := boot + run; vm > 0 {
		lm.vmSeconds.Add(ctx, vm.Seconds(), attrs)
	}
	if redactions > 0 {
		lm.redactions.Add(ctx, int64(redactions), attrs)
	}
}

// outcomeLabel is LaunchOutcome as a metric/log value: "boot_failure", ...
func outcomeLabel(o launcherv1.LaunchOutcome) string {
	return strings.ToLower(strings.TrimPrefix(o.String(), "LAUNCH_OUTCOME_"))
}
