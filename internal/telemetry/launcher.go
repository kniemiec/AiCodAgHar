package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Launcher is container-launcher's metric catalogue (MT-11, MT-12, MT-16).
// A nil *Launcher records nothing.
type Launcher struct {
	reg         *registry
	invocations *Counter
	queueWait   *Histogram
	boot        *Histogram
	guestRun    *Histogram
	vmSeconds   *FloatCounter
	redactions  *Counter
}

// NewLauncher builds the catalogue on mp. queueDepth backs the
// launcher_queue_depth gauge (requests waiting for the VM slot).
func NewLauncher(mp metric.MeterProvider, queueDepth func() int64) *Launcher {
	reg := newRegistry(mp)
	m := &Launcher{
		reg:         reg,
		invocations: reg.counter("launcher_invocations_total", "Launch invocations by outcome.", "outcome", "step_type"),
		queueWait:   reg.histogram("launcher_queue_wait_seconds", "Time a Launch waited for the VM slot.", "s", launcherBuckets, "step_type"),
		boot:        reg.histogram("launcher_boot_seconds", "VM slot acquired to InstanceStart.", "s", launcherBuckets, "step_type"),
		guestRun:    reg.histogram("launcher_guest_run_seconds", "InstanceStart to guest completion.", "s", launcherBuckets, "step_type"),
		vmSeconds:   reg.floatCounter("launcher_vm_seconds_total", "VM time consumed (boot + guest run).", "s", "step_type"),
		redactions:  reg.counter("launcher_redactions_total", "Secret values replaced in guest output.", "step_type"),
	}
	depth := reg.gauge("launcher_queue_depth", "Launch requests waiting for the VM slot.")
	if _, err := reg.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(depth, queueDepth())
		return nil
	}, depth); err != nil {
		otel.Handle(err)
	}
	return m
}

// Catalogue lists the instruments NewLauncher defines.
func (m *Launcher) Catalogue() []Def { return m.reg.defs }

// Invocation records one finished Launch.
func (m *Launcher) Invocation(ctx context.Context, stepType, outcome string, queueWait, boot, guestRun time.Duration, redactions int) {
	if m == nil {
		return
	}
	st := attribute.String("step_type", launcherStepType(stepType))
	m.invocations.Add(ctx, 1, st, attribute.String("outcome", outcome))
	m.queueWait.Record(ctx, queueWait.Seconds(), st)
	if boot > 0 {
		m.boot.Record(ctx, boot.Seconds(), st)
	}
	if guestRun > 0 {
		m.guestRun.Record(ctx, guestRun.Seconds(), st)
	}
	if vm := (boot + guestRun).Seconds(); vm > 0 {
		m.vmSeconds.Add(ctx, vm, st)
	}
	if redactions > 0 {
		m.redactions.Add(ctx, int64(redactions), st)
	}
}

// launcherStepType bounds the caller-supplied step type to the two the
// contract defines.
func launcherStepType(s string) string {
	switch s {
	case "agent-stage", "deterministic-step":
		return s
	}
	return Other
}
