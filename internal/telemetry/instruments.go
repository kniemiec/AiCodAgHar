package telemetry

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/kniemiec/AiCodAgHar/internal/buildinfo"
)

// meterName is the instrumentation scope of every catalogue instrument.
const meterName = "github.com/kniemiec/AiCodAgHar/internal/telemetry"

// AllowedKeys is the closed set of metric attribute keys (CC-2). An
// instrument may declare only keys from it; NewMeterProvider's view drops
// anything else as a backstop. Per-task identifiers (task_id,
// invocation_id, trace_id) are deliberately absent.
var AllowedKeys = []string{
	"component", "step_type", "step_id", "phase", "outcome", "status", "error_class",
	"retryable", "dependency", "target_repo", "method", "code", "round", "version",
}

// Def describes one catalogue instrument, for the CC-2 test.
type Def struct {
	Name string
	Keys []string
}

// registry creates instruments and remembers their definitions.
type registry struct {
	meter metric.Meter
	defs  []Def
}

func newRegistry(mp metric.MeterProvider) *registry {
	return &registry{meter: mp.Meter(meterName)}
}

func (r *registry) define(name string, keys []string) map[string]bool {
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !slices.Contains(AllowedKeys, k) {
			panic(fmt.Sprintf("telemetry: %s declares label %q outside AllowedKeys", name, k))
		}
		allowed[k] = true
	}
	r.defs = append(r.defs, Def{Name: name, Keys: keys})
	return allowed
}

// Counter is an int64 counter that only ever records its declared keys.
type Counter struct {
	keys map[string]bool
	c    metric.Int64Counter
}

func (r *registry) counter(name, desc string, keys ...string) *Counter {
	c, err := r.meter.Int64Counter(name, metric.WithDescription(desc))
	if err != nil {
		otel.Handle(err)
	}
	return &Counter{keys: r.define(name, keys), c: c}
}

// Add increments the counter by n.
func (c *Counter) Add(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	c.c.Add(ctx, n, metric.WithAttributeSet(filter(c.keys, attrs)))
}

// FloatCounter is a float64 counter (e.g. VM-seconds).
type FloatCounter struct {
	keys map[string]bool
	c    metric.Float64Counter
}

func (r *registry) floatCounter(name, desc, unit string, keys ...string) *FloatCounter {
	c, err := r.meter.Float64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
	}
	return &FloatCounter{keys: r.define(name, keys), c: c}
}

// Add increments the counter by v.
func (c *FloatCounter) Add(ctx context.Context, v float64, attrs ...attribute.KeyValue) {
	c.c.Add(ctx, v, metric.WithAttributeSet(filter(c.keys, attrs)))
}

// Histogram is a float64 histogram with explicit bucket boundaries.
type Histogram struct {
	keys map[string]bool
	h    metric.Float64Histogram
}

func (r *registry) histogram(name, desc, unit string, bounds []float64, keys ...string) *Histogram {
	h, err := r.meter.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit),
		metric.WithExplicitBucketBoundaries(bounds...))
	if err != nil {
		otel.Handle(err)
	}
	return &Histogram{keys: r.define(name, keys), h: h}
}

// Record adds one observation.
func (h *Histogram) Record(ctx context.Context, v float64, attrs ...attribute.KeyValue) {
	h.h.Record(ctx, v, metric.WithAttributeSet(filter(h.keys, attrs)))
}

// gauge creates an async int64 gauge; the caller registers its callback.
func (r *registry) gauge(name, desc string, keys ...string) metric.Int64ObservableGauge {
	g, err := r.meter.Int64ObservableGauge(name, metric.WithDescription(desc))
	if err != nil {
		otel.Handle(err)
	}
	r.define(name, keys)
	return g
}

func (r *registry) floatGauge(name, desc, unit string, keys ...string) metric.Float64ObservableGauge {
	g, err := r.meter.Float64ObservableGauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
	}
	r.define(name, keys)
	return g
}

// filter keeps only the declared keys: an undeclared attribute passed at a
// call site is dropped rather than creating a new series.
func filter(keys map[string]bool, attrs []attribute.KeyValue) attribute.Set {
	kept := attrs[:0:0]
	for _, a := range attrs {
		if keys[string(a.Key)] {
			kept = append(kept, a)
		}
	}
	return attribute.NewSet(kept...)
}

// Histogram bucket boundaries, in seconds (design §6).
var (
	taskBuckets     = []float64{30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800}
	stepBuckets     = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800}
	roundBuckets    = []float64{1, 2, 3, 4, 5}
	launcherBuckets = []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1200, 1800}
	depBuckets      = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	approvalBuckets = []float64{60, 300, 900, 3600, 4 * 3600, 8 * 3600, 24 * 3600, 3 * 24 * 3600, 7 * 24 * 3600}
)

// Common holds the instruments both binaries emit.
type Common struct {
	reg     *registry
	dropped *Counter
}

// NewCommon registers harness_build_info (HL-3) and
// harness_telemetry_dropped_total (CC-4) for component.
func NewCommon(mp metric.MeterProvider, component string) *Common {
	reg := newRegistry(mp)
	c := &Common{reg: reg}
	c.dropped = reg.counter("harness_telemetry_dropped_total", "Telemetry export errors (dropped data).", "component")
	info := reg.gauge("harness_build_info", "Always 1; labels identify the running build.", "version", "component")
	attrs := metric.WithAttributes(attribute.String("version", buildinfo.Version), attribute.String("component", component))
	if _, err := reg.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(info, 1, attrs)
		return nil
	}, info); err != nil {
		otel.Handle(err)
	}
	return c
}

// Catalogue lists the instruments NewCommon defines.
func (c *Common) Catalogue() []Def { return c.reg.defs }

// Other replaces a label value outside a Bounded set.
const Other = "other"

// Bounded caps a free-form label's cardinality (CC-2, Q6). With an allow
// list, values outside it become Other; without one, the first max distinct
// values pass and later ones become Other.
type Bounded struct {
	mu    sync.Mutex
	allow map[string]bool
	seen  map[string]bool
	max   int
}

// DefaultBoundedMax is the first-seen cap when no allow list is configured.
const DefaultBoundedMax = 64

// NewBounded builds a Bounded. An empty allow list means first-seen-up-to-max.
func NewBounded(allow []string, max int) *Bounded {
	b := &Bounded{seen: map[string]bool{}, max: max}
	if len(allow) > 0 {
		b.allow = map[string]bool{}
		for _, v := range allow {
			b.allow[v] = true
		}
	}
	return b
}

// Value returns v, or Other when v is outside the bound.
func (b *Bounded) Value(v string) string {
	if b.allow != nil {
		if b.allow[v] {
			return v
		}
		return Other
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[v] {
		return v
	}
	if len(b.seen) >= b.max {
		return Other
	}
	b.seen[v] = true
	return v
}

// ParseList splits a comma-separated env value, dropping blanks.
func ParseList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
