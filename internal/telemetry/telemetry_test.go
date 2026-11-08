package telemetry

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// forbiddenKeys are per-task/per-request identifiers that must never become
// metric labels (CC-2).
var forbiddenKeys = []string{"task_id", "invocation_id", "trace_id", "span_id", "issue_number", "pr_number"}

// TestCatalogue_LabelsBounded is CC-2's catalogue test: every instrument of
// every catalogue declares only allowed keys, none of them per-task.
func TestCatalogue_LabelsBounded(t *testing.T) {
	mp := noop.NewMeterProvider()
	var defs []Def
	defs = append(defs, NewCommon(mp, "test").Catalogue()...)
	defs = append(defs, NewControlPlane(mp, ControlPlaneOptions{}).Catalogue()...)
	defs = append(defs, NewLauncher(mp, func() int64 { return 0 }).Catalogue()...)
	if len(defs) < 30 {
		t.Fatalf("catalogue has %d instruments; the test proves nothing if the catalogue isn't enumerated", len(defs))
	}

	names := map[string]bool{}
	for _, d := range defs {
		if names[d.Name] {
			t.Errorf("duplicate instrument %s", d.Name)
		}
		names[d.Name] = true
		for _, k := range d.Keys {
			if !slices.Contains(AllowedKeys, k) {
				t.Errorf("%s: label %q not in AllowedKeys", d.Name, k)
			}
			if slices.Contains(forbiddenKeys, k) {
				t.Errorf("%s: per-task label %q", d.Name, k)
			}
		}
	}
	for _, k := range forbiddenKeys {
		if slices.Contains(AllowedKeys, k) {
			t.Errorf("AllowedKeys contains per-task key %q", k)
		}
	}
}

// TestView_DropsUndeclaredKeys: both guards hold — the instrument wrapper
// drops an attribute its definition doesn't declare, and the provider's
// view drops a non-allowed key even from an instrument created around the
// catalogue.
func TestView_DropsUndeclaredKeys(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := NewMeterProvider(nil, reader)
	ctx := context.Background()

	reg := newRegistry(mp)
	c := reg.counter("test_wrapped_total", "", "outcome")
	c.Add(ctx, 1, attribute.String("outcome", "ok"), attribute.String("task_id", "task-1"))

	raw, err := mp.Meter("raw").Int64Counter("test_raw_total")
	if err != nil {
		t.Fatal(err)
	}
	raw.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "running"), attribute.String("task_id", "task-1")))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]attribute.Set{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				seen[m.Name] = dp.Attributes
			}
		}
	}
	for name, kept := range map[string]string{"test_wrapped_total": "outcome", "test_raw_total": "status"} {
		set, ok := seen[name]
		if !ok {
			t.Fatalf("%s not collected; the test proves nothing", name)
		}
		if _, has := set.Value(attribute.Key(kept)); !has {
			t.Errorf("%s lost its allowed key %q: %v", name, kept, set.ToSlice())
		}
		if _, has := set.Value("task_id"); has {
			t.Errorf("%s kept task_id: %v", name, set.ToSlice())
		}
	}
}

func TestDefine_PanicsOnUnknownKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("declaring task_id did not panic")
		}
	}()
	newRegistry(noop.NewMeterProvider()).counter("bad_total", "", "task_id")
}

func TestBounded(t *testing.T) {
	b := NewBounded(nil, 2)
	for _, tc := range []struct{ in, want string }{{"a", "a"}, {"b", "b"}, {"c", Other}, {"a", "a"}} {
		if got := b.Value(tc.in); got != tc.want {
			t.Errorf("first-seen Value(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	allow := NewBounded([]string{"acme/widgets"}, 64)
	if allow.Value("acme/widgets") != "acme/widgets" || allow.Value("evil/repo") != Other {
		t.Error("allow-list bound did not map outsiders to other")
	}
	if got := ParseList(" a, ,b ,"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("ParseList = %v", got)
	}
}

func TestSetup_NoopWithoutEndpoint(t *testing.T) {
	t.Setenv(EndpointEnv, "")
	tel := Setup(context.Background(), "test")
	if len(tel.LogHandlers()) != 0 {
		t.Error("no-op telemetry returned a log bridge")
	}
	if _, ok := tel.MeterProvider.(noop.MeterProvider); !ok {
		t.Errorf("MeterProvider = %T, want noop", tel.MeterProvider)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestErrorHandler_CountsDrops(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	common := NewCommon(NewMeterProvider(nil, reader), "test")
	h := newErrorHandler("test", common.dropped)
	for range 3 {
		h.Handle(errors.New("export failed"))
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var dropped int64 = -1
	var buildInfo bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "harness_telemetry_dropped_total":
				dropped = m.Data.(metricdata.Sum[int64]).DataPoints[0].Value
			case "harness_build_info":
				buildInfo = m.Data.(metricdata.Gauge[int64]).DataPoints[0].Value == 1
			}
		}
	}
	if dropped != 3 {
		t.Errorf("harness_telemetry_dropped_total = %d, want 3", dropped)
	}
	if !buildInfo {
		t.Error("harness_build_info not reported as 1")
	}
}
