package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

type captureSink struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureSink) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureSink) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, r.Message)
	return nil
}
func (c *captureSink) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureSink) WithGroup(string) slog.Handler      { return c }

func jsonLogger(buf *bytes.Buffer, extra ...slog.Handler) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(contextHandler{fanout{primary: base, sinks: extra}})
}

func TestContextAttrsAppended(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf)
	ctx := WithStep(WithRound(WithTask(context.Background(), "t1"), 2), "review", "t1-r2-review")
	l.InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"task_id": "t1", "round": float64(2), "step_id": "review", "invocation_id": "t1-r2-review"} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestContextAttrsOmittedWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	jsonLogger(&buf).InfoContext(context.Background(), "hello")
	for _, k := range []string{"task_id", "round", "step_id", "invocation_id"} {
		if strings.Contains(buf.String(), `"`+k+`"`) {
			t.Errorf("unexpected %s in %s", k, buf.String())
		}
	}
}

func TestSensitiveRecordsStayOnStderr(t *testing.T) {
	var buf bytes.Buffer
	sink := &captureSink{}
	l := jsonLogger(&buf, sink)
	ctx := context.Background()
	l.InfoContext(ctx, "ordinary")
	l.WarnContext(ctx, "secret-bearing", Sensitive(), "tail", "x")

	if !strings.Contains(buf.String(), "secret-bearing") {
		t.Error("sensitive record missing from stderr")
	}
	if len(sink.msgs) != 1 || sink.msgs[0] != "ordinary" {
		t.Errorf("central sink got %v, want only the ordinary record", sink.msgs)
	}
}

func TestSensitiveViaWithAttrsLoggerStillFiltered(t *testing.T) {
	var buf bytes.Buffer
	sink := &captureSink{}
	l := jsonLogger(&buf, sink).With("k", "v")
	l.InfoContext(context.Background(), "x", Sensitive())
	if len(sink.msgs) != 0 {
		t.Errorf("sink got %v", sink.msgs)
	}
}

func TestTraceIDsFromContext(t *testing.T) {
	var buf bytes.Buffer
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 1},
		SpanID:     trace.SpanID{0x0c, 2},
		TraceFlags: trace.FlagsSampled,
	})
	jsonLogger(&buf).InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "traced")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["trace_id"] != sc.TraceID().String() || rec["span_id"] != sc.SpanID().String() {
		t.Errorf("trace_id/span_id = %v/%v, want %s/%s", rec["trace_id"], rec["span_id"], sc.TraceID(), sc.SpanID())
	}

	buf.Reset()
	jsonLogger(&buf).InfoContext(context.Background(), "untraced")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("trace_id on a record without a span: %s", buf.String())
	}
}

// TestExtraSinksFollowLogLevel: an extra sink that accepts every level (as
// the OTLP bridge does) still only receives records at or above LOG_LEVEL.
func TestExtraSinksFollowLogLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	sink := &captureSink{}
	l := slog.New(NewHandler(&bytes.Buffer{}, "test", sink))
	l.DebugContext(context.Background(), "chatter")
	l.InfoContext(context.Background(), "lifecycle")
	if len(sink.msgs) != 1 || sink.msgs[0] != "lifecycle" {
		t.Errorf("extra sink got %v, want only the info record", sink.msgs)
	}
}
