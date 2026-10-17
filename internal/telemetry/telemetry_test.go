package telemetry

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestDeclare_RejectsDisallowedKey(t *testing.T) {
	for _, key := range []string{"task_id", "invocation_id", "trace_id"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewCounter with key %q did not panic", key)
				}
			}()
			NewCounter(sdkmetric.NewMeterProvider().Meter("t"), "test_bad_"+key, "", "", key)
		}()
	}
}

// The SDK view is the CC-2 backstop: even an attribute passed at record
// time that the instrument never declared is dropped.
func TestAllowedKeysView_DropsUnknownKeys(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(AllowedKeysView()))
	c := NewCounter(mp.Meter("t"), "test_view_total", "", "", "outcome")
	c.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", "ok"), attribute.String("task_id", "t-1")))
	h := NewHistogram(mp.Meter("t"), "test_view_seconds", "", "s", []float64{1, 2}, "outcome")
	h.Record(context.Background(), 1.5, metric.WithAttributes(attribute.String("outcome", "ok")))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			attrs := d.DataPoints[0].Attributes
			if _, ok := attrs.Value("task_id"); ok {
				t.Errorf("task_id survived the view: %v", attrs.ToSlice())
			}
			if v, _ := attrs.Value("outcome"); v.AsString() != "ok" {
				t.Errorf("outcome dropped: %v", attrs.ToSlice())
			}
		case metricdata.Histogram[float64]:
			// The view must not discard the instrument's own boundaries.
			if got := d.DataPoints[0].Bounds; len(got) != 2 || got[0] != 1 || got[1] != 2 {
				t.Errorf("histogram bounds = %v, want [1 2]", got)
			}
		}
	}
}

func TestCatalogue_KeysAreAllowed(t *testing.T) {
	NewCounter(sdkmetric.NewMeterProvider().Meter("t"), "test_catalogue_total", "", "", "outcome", "step_type")
	found := false
	for _, in := range Catalogue() {
		if in.Name == "test_catalogue_total" {
			found = true
			if strings.Join(in.Keys, ",") != "outcome,step_type" {
				t.Errorf("keys = %v", in.Keys)
			}
		}
	}
	if !found {
		t.Error("declared instrument missing from the catalogue")
	}
}

func TestBounded(t *testing.T) {
	allow := NewBounded([]string{"a", "b"}, DefaultBoundedMax)
	if allow.Value("a") != "a" || allow.Value("z") != Other {
		t.Errorf("allow-list mode: a=%q z=%q", allow.Value("a"), allow.Value("z"))
	}
	first := NewBounded(nil, 2)
	for _, v := range []string{"x", "y", "x"} {
		if first.Value(v) != v {
			t.Errorf("first-seen: %q became %q", v, first.Value(v))
		}
	}
	if first.Value("z") != Other {
		t.Errorf("third distinct value = %q, want %q", first.Value("z"), Other)
	}
	if (*Bounded)(nil).Value("q") != "q" {
		t.Error("nil Bounded must pass values through")
	}
}

func TestSetup_NoEndpointIsNoop(t *testing.T) {
	t.Setenv(EndpointEnv, "")
	tel, err := Setup(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if tel.LogHandler != nil {
		t.Error("LogHandler set without an endpoint")
	}
	NewCounter(tel.Meter(), "test_noop_total", "", "").Add(context.Background(), 1)
	tel.RegisterBuildInfo()
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// CC-4: with the Collector unreachable, recording metrics and logs never
// blocks the caller, and shutdown is bounded by its context.
func TestSetup_UnreachableCollectorDoesNotBlock(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here any more
	t.Setenv(EndpointEnv, "http://"+addr)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "50")

	tel, err := Setup(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if tel.LogHandler == nil {
		t.Fatal("LogHandler not set with an endpoint")
	}
	c := NewCounter(tel.Meter(), "test_dead_collector_total", "", "", "outcome")
	log := slog.New(tel.LogHandler)

	start := time.Now()
	for i := range 5000 {
		c.Add(context.Background(), 1, Attrs("outcome", "ok"))
		log.InfoContext(context.Background(), "record", "i", i)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("recording against a dead Collector took %v", d)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start = time.Now()
	_ = tel.Shutdown(ctx) // an export error is expected; only the time matters
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Shutdown took %v, want it bounded by its 500ms context", d)
	}
}
