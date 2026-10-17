// Package telemetry bootstraps OpenTelemetry for both binaries
// (platform-observability design §6, §9): an OTLP metrics pipeline and an
// OTLP log sink for internal/logging, both no-ops unless
// OTEL_EXPORTER_OTLP_ENDPOINT is set, so plain `go test` and bare `go run`
// stay quiet. It also owns the metric-attribute guard (CC-2): instruments
// are created through NewCounter/NewHistogram/DeclareGauge, which reject any
// attribute key outside AllowedKeys, and the SDK view drops any other key as
// a backstop.
//
// Exporters use the SDK's periodic reader and batch processor (bounded
// queues, asynchronous export), so a dead Collector never blocks a task
// (CC-4). Export errors go to a rate-limited stderr-only logger, never back
// through the OTLP log sink.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/kniemiec/AiCodAgHar/internal/buildinfo"
)

// EndpointEnv enables telemetry export when set.
const EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// AllowedKeys is the closed set of metric attribute keys (CC-2): no
// per-task, per-invocation or per-trace identifier may ever be a label.
var AllowedKeys = []string{
	"component", "version", "step_type", "step_id", "phase", "outcome", "status",
	"error_class", "retryable", "dependency", "target_repo", "method", "code", "round",
}

// Histogram bucket boundaries, in seconds (design §6).
var (
	TaskSecondsBuckets     = []float64{30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800}
	StepSecondsBuckets     = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800}
	LauncherSecondsBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1200, 1800}
	RequestSecondsBuckets  = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	RoundBuckets           = []float64{1, 2, 3, 4, 5}
)

// Telemetry is one process's OTel wiring.
type Telemetry struct {
	// MeterProvider is a no-op provider when export is disabled.
	MeterProvider metric.MeterProvider
	// LogHandler is the OTLP log sink for logging.Setup; nil when disabled.
	LogHandler slog.Handler

	component string
	shutdown  []func(context.Context) error
	dropped   metric.Int64Counter
}

// Setup builds component's telemetry from the standard OTEL_* environment.
// With no endpoint it returns no-op providers and a nil LogHandler.
func Setup(ctx context.Context, component string) (*Telemetry, error) {
	t := &Telemetry{MeterProvider: noop.NewMeterProvider(), component: component}
	if os.Getenv(EndpointEnv) == "" {
		t.initDropped()
		return t, nil
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(), // OTEL_RESOURCE_ATTRIBUTES, e.g. deployment.environment
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(component), semconv.ServiceVersion(buildinfo.Version)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}

	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithView(AllowedKeysView()),
	)
	t.MeterProvider = mp
	t.shutdown = append(t.shutdown, mp.Shutdown)

	logExp, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: log exporter: %w", err), t.Shutdown(ctx))
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)))
	t.shutdown = append(t.shutdown, lp.Shutdown)
	t.LogHandler = otelslog.NewHandler(component, otelslog.WithLoggerProvider(lp), otelslog.WithVersion(buildinfo.Version))

	t.initDropped()
	otel.SetErrorHandler(newErrorHandler(component, t.dropped))
	return t, nil
}

func (t *Telemetry) initDropped() {
	t.dropped = NewCounter(t.Meter(), "harness_telemetry_dropped_total",
		"Telemetry export errors (dropped batches or records).", "{error}", "component")
}

// Meter returns the meter all of a process's instruments are created from.
func (t *Telemetry) Meter() metric.Meter {
	return t.MeterProvider.Meter("github.com/kniemiec/AiCodAgHar")
}

// Shutdown flushes and stops every exporter. Bound ctx (design §8: 5 s).
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, fn := range t.shutdown {
		errs = append(errs, fn(ctx))
	}
	t.shutdown = nil
	return errors.Join(errs...)
}

// AllowedKeysView drops every attribute key outside AllowedKeys from every
// instrument — the CC-2 backstop behind NewCounter's construction check.
func AllowedKeysView() sdkmetric.View {
	keys := make([]attribute.Key, len(AllowedKeys))
	for i, k := range AllowedKeys {
		keys[i] = attribute.Key(k)
	}
	return sdkmetric.NewView(sdkmetric.Instrument{Name: "*"}, sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(keys...)})
}

// Instrument is one catalogue entry: a metric and the attribute keys it uses.
type Instrument struct {
	Name string
	Keys []string
}

var (
	catalogueMu sync.Mutex
	catalogue   = map[string]Instrument{}
)

// Catalogue returns every instrument declared so far, sorted by name.
func Catalogue() []Instrument {
	catalogueMu.Lock()
	defer catalogueMu.Unlock()
	out := make([]Instrument, 0, len(catalogue))
	for _, in := range catalogue {
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b Instrument) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out
}

// declare records name's keys in the catalogue, panicking on any key outside
// AllowedKeys: a disallowed label is a programming error, caught by the
// first test that builds the instrument.
func declare(name string, keys []string) {
	for _, k := range keys {
		if !slices.Contains(AllowedKeys, k) {
			panic(fmt.Sprintf("telemetry: metric %s: attribute key %q is not allowed (CC-2)", name, k))
		}
	}
	catalogueMu.Lock()
	catalogue[name] = Instrument{Name: name, Keys: slices.Clone(keys)}
	catalogueMu.Unlock()
}

// NewCounter creates an Int64Counter whose attributes are limited to keys.
func NewCounter(m metric.Meter, name, desc, unit string, keys ...string) metric.Int64Counter {
	declare(name, keys)
	c, err := m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
	}
	return c
}

// NewFloatCounter creates a Float64Counter whose attributes are limited to keys.
func NewFloatCounter(m metric.Meter, name, desc, unit string, keys ...string) metric.Float64Counter {
	declare(name, keys)
	c, err := m.Float64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
	}
	return c
}

// NewHistogram creates a Float64Histogram with explicit bucket boundaries
// whose attributes are limited to keys.
func NewHistogram(m metric.Meter, name, desc, unit string, buckets []float64, keys ...string) metric.Float64Histogram {
	declare(name, keys)
	h, err := m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit),
		metric.WithExplicitBucketBoundaries(buckets...))
	if err != nil {
		otel.Handle(err)
	}
	return h
}

// DeclareGauge records an observable gauge's keys in the catalogue; the
// caller creates the gauge itself (it needs a callback).
func DeclareGauge(name string, keys ...string) { declare(name, keys) }

// RegisterBuildInfo exports harness_build_info{component,version} = 1
// (HL-3), so a dashboard can show which build each process runs and an alert
// can notice a process that stopped exporting.
func (t *Telemetry) RegisterBuildInfo() {
	DeclareGauge("harness_build_info", "component", "version")
	attrs := metric.WithAttributes(attribute.String("component", t.component), attribute.String("version", buildinfo.Version))
	if _, err := t.Meter().Int64ObservableGauge("harness_build_info",
		metric.WithDescription("Always 1; labels identify the running build."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, attrs)
			return nil
		})); err != nil {
		otel.Handle(err)
	}
}

// Attrs builds a measurement option from key/value string pairs.
func Attrs(kv ...string) metric.MeasurementOption {
	attrs := make([]attribute.KeyValue, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		attrs = append(attrs, attribute.String(kv[i], kv[i+1]))
	}
	return metric.WithAttributes(attrs...)
}

// errorHandler logs OTel's internal errors (export failures, mostly) to
// stderr only — routing them through the OTLP log sink would feed an export
// failure back into the exporter — at most once per errorLogInterval, and
// counts every one in harness_telemetry_dropped_total.
type errorHandler struct {
	log       *slog.Logger
	component string
	dropped   metric.Int64Counter

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

const errorLogInterval = 30 * time.Second

func newErrorHandler(component string, dropped metric.Int64Counter) *errorHandler {
	return &errorHandler{
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", component),
		component: component,
		dropped:   dropped,
	}
}

func (h *errorHandler) Handle(err error) {
	h.dropped.Add(context.Background(), 1, Attrs("component", h.component))
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.last) < errorLogInterval {
		h.suppressed++
		return
	}
	h.log.Warn("telemetry export error", "error", err, "suppressed_since_last", h.suppressed)
	h.last = time.Now()
	h.suppressed = 0
}
