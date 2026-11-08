// Package telemetrytest reads metrics recorded in tests: a MeterProvider
// with the production attribute view over a ManualReader, plus lookups by
// metric name and label subset.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// Reader collects from a test MeterProvider.
type Reader struct {
	*sdkmetric.ManualReader
	Provider *sdkmetric.MeterProvider
}

// New returns a MeterProvider built like production (telemetry.NewMeterProvider)
// and the reader attached to it.
func New() *Reader {
	r := sdkmetric.NewManualReader()
	return &Reader{ManualReader: r, Provider: telemetry.NewMeterProvider(nil, r)}
}

// Collect gathers everything recorded so far.
func (r *Reader) Collect(t testing.TB) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.ManualReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

// Point is one data point flattened: its labels and value (counter/gauge
// value, or histogram count and sum).
type Point struct {
	Attrs map[string]string
	Value float64
	Count uint64
	Sum   float64
}

// Points returns every data point of the named metric.
func (r *Reader) Points(t testing.TB, name string) []Point {
	t.Helper()
	var out []Point
	for _, sm := range r.Collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{Attrs: attrs(dp.Attributes), Value: float64(dp.Value)})
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{Attrs: attrs(dp.Attributes), Value: dp.Value})
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{Attrs: attrs(dp.Attributes), Value: float64(dp.Value)})
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{Attrs: attrs(dp.Attributes), Value: dp.Value})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{Attrs: attrs(dp.Attributes), Count: dp.Count, Sum: dp.Sum})
				}
			}
		}
	}
	return out
}

// Value sums counter/gauge values (histograms: observation counts) over the
// points of name whose labels include every key/value in match.
func (r *Reader) Value(t testing.TB, name string, match map[string]string) float64 {
	t.Helper()
	var total float64
	for _, p := range r.Points(t, name) {
		if !matches(p.Attrs, match) {
			continue
		}
		if p.Count > 0 {
			total += float64(p.Count)
		} else {
			total += p.Value
		}
	}
	return total
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func attrs(s attribute.Set) map[string]string {
	out := map[string]string{}
	for _, kv := range s.ToSlice() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}
