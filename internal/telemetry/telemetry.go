// Package telemetry bootstraps OpenTelemetry for both binaries
// (platform-observability design §6, §9): an OTLP metric exporter and the
// OTLP log bridge, a closed metric catalogue whose labels can't exceed a
// fixed key set (CC-2), and the EventSink that derives task metrics from
// committed task_events.
//
// Nothing is exported unless OTEL_EXPORTER_OTLP_ENDPOINT is set: plain
// `go test` and bare `go run` stay quiet. All exporter options come from the
// standard OTEL_* variables. Exports are asynchronous and bounded (periodic
// metric reader, batch log processor), so a dead Collector drops telemetry
// instead of slowing a task (CC-4).
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
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

	"github.com/kniemiec/AiCodAgHar/internal/buildinfo"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// EndpointEnv is the variable whose presence turns exporting on.
const EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Telemetry is one process's providers. The zero value is not usable; use
// Setup (or Noop in tests).
type Telemetry struct {
	// MeterProvider is what the metric catalogues are built from. It is a
	// no-op provider when exporting is off.
	MeterProvider metric.MeterProvider

	logHandler slog.Handler
	shutdown   []func(context.Context) error
}

// Noop returns a Telemetry that records and exports nothing.
func Noop() *Telemetry {
	return &Telemetry{MeterProvider: noop.NewMeterProvider()}
}

// Setup builds the providers for component ("control-plane",
// "container-launcher"). It never fails: a configuration error is logged and
// yields Noop, because telemetry must never stop the process (CC-4). Call it
// before logging.Setup and pass LogHandlers() there.
func Setup(ctx context.Context, component string) *Telemetry {
	if os.Getenv(EndpointEnv) == "" {
		return Noop()
	}
	t, err := setup(ctx, component)
	if err != nil {
		slog.WarnContext(ctx, "telemetry disabled: setup failed", "error", err)
		return Noop()
	}
	return t
}

func setup(ctx context.Context, component string) (*Telemetry, error) {
	res, err := newResource(component)
	if err != nil {
		return nil, err
	}

	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	mp := NewMeterProvider(res, sdkmetric.NewPeriodicReader(metricExp))

	logExp, err := otlploggrpc.New(ctx)
	if err != nil {
		_ = mp.Shutdown(ctx)
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)))

	common := NewCommon(mp, component)
	otel.SetMeterProvider(mp)
	otel.SetErrorHandler(newErrorHandler(component, common.dropped))

	return &Telemetry{
		MeterProvider: mp,
		logHandler:    otelslog.NewHandler(component, otelslog.WithLoggerProvider(lp)),
		shutdown:      []func(context.Context) error{lp.Shutdown, mp.Shutdown},
	}, nil
}

// newResource identifies the process (CC-8): service.name = component,
// service.version, host.name, plus whatever OTEL_RESOURCE_ATTRIBUTES adds
// (deployment.environment).
func newResource(component string) (*resource.Resource, error) {
	host, _ := os.Hostname()
	return resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", component),
		attribute.String("service.version", buildinfo.Version),
		attribute.String("host.name", host),
	))
}

// NewMeterProvider builds an SDK MeterProvider over readers with the
// catalogue-wide attribute allow-list view (CC-2's hard backstop). Exported
// so tests can attach a ManualReader.
func NewMeterProvider(res *resource.Resource, readers ...sdkmetric.Reader) *sdkmetric.MeterProvider {
	keys := make([]attribute.Key, len(AllowedKeys))
	for i, k := range AllowedKeys {
		keys[i] = attribute.Key(k)
	}
	opts := []sdkmetric.Option{sdkmetric.WithView(sdkmetric.NewView(
		sdkmetric.Instrument{Name: "*"},
		sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(keys...)},
	))}
	if res != nil {
		opts = append(opts, sdkmetric.WithResource(res))
	}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	return sdkmetric.NewMeterProvider(opts...)
}

// LogHandlers returns the extra slog sinks for logging.Setup: the OTLP log
// bridge when exporting is on, nothing otherwise.
func (t *Telemetry) LogHandlers() []slog.Handler {
	if t.logHandler == nil {
		return nil
	}
	return []slog.Handler{t.logHandler}
}

// Shutdown flushes and stops the exporters, bounded by ctx.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, fn := range t.shutdown {
		errs = append(errs, fn(ctx))
	}
	return errors.Join(errs...)
}

// errorLogInterval rate-limits the OTel error handler's log lines.
const errorLogInterval = 10 * time.Second

// newErrorHandler routes OTel's internal errors (failed exports, dropped
// batches) to a stderr-only logger — never the bridge, which would feed
// export failures back into the exporter — at most once per
// errorLogInterval, and counts every one in harness_telemetry_dropped_total.
func newErrorHandler(component string, dropped *Counter) otel.ErrorHandler {
	log := slog.New(logging.NewHandler(os.Stderr, component))
	var mu sync.Mutex
	var last time.Time
	var suppressed int
	return otel.ErrorHandlerFunc(func(err error) {
		dropped.Add(context.Background(), 1, attribute.String("component", component))
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < errorLogInterval {
			suppressed++
			return
		}
		log.Warn("telemetry export error", "error", err, "suppressed_since_last", suppressed)
		last, suppressed = time.Now(), 0
	})
}
