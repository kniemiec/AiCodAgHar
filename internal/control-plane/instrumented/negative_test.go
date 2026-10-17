package instrumented_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// captureHandler is an in-memory central sink (stands in for the OTLP
// bridge): it renders every record it is handed, attributes included.
type captureHandler struct {
	mu    *sync.Mutex
	lines *[]string
	attrs []slog.Attr
}

func newCapture() captureHandler { return captureHandler{mu: &sync.Mutex{}, lines: &[]string{}} }

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	*h.lines = append(*h.lines, b.String())
	h.mu.Unlock()
	return nil
}
func (h captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return captureHandler{mu: h.mu, lines: h.lines, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}
func (h captureHandler) WithGroup(string) slog.Handler { return h }

func (h captureHandler) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(*h.lines, "\n")
}

// Success criterion 11 (LG-6, SEC-1, SEC-2): a task's credential, its
// description and the agent's output never reach the central log sink, a
// metric attribute or an event payload. (Span attributes join in PR E.)
func TestNoSensitiveContentLeavesTheHost(t *testing.T) {
	const (
		description = "DESCRIPTION-SECRET-7f3a"
		agentOutput = "AGENT-OUTPUT-SECRET-91bc"
	)
	t.Setenv("LOG_LEVEL", "debug")
	central := newCapture()
	var stderr syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(&stderr, "control-plane", central)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t)
	h.policy.EvaluateFunc = policyFor()
	var tokens []string
	var mu sync.Mutex
	h.launcher.LaunchFunc = func(_ context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
		mu.Lock()
		tokens = append(tokens, req.Env["FORGEJO_TOKEN"])
		mu.Unlock()
		return launcherclient.RunResult{InvocationID: req.InvocationID, Status: "completed", Outcome: launcherclient.OutcomeCompleted}, nil
	}
	h.launcher.ExtractLogsFunc = func(context.Context, string) (launcherclient.LogBundle, error) {
		return launcherclient.LogBundle{
			Stdout:               []byte("agent says " + agentOutput),
			StructuredResultJSON: []byte(`{"has_blocking_finding":false,"summary":"` + agentOutput + `"}`),
		}, nil
	}
	_, done, err := h.runner.Start(context.Background(), domain.Task{ID: "task-secret", TargetRepo: "acme/widgets", Description: description})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if len(tokens) == 0 || tokens[0] == "" {
		t.Fatal("no credential reached the launcher; the test proves nothing")
	}
	needles := append([]string{description, agentOutput}, tokens...)

	// Control: the local stderr sink does get the sensitive debug record,
	// so the absence below is the split working, not the content missing.
	if !strings.Contains(stderr.String(), agentOutput) {
		t.Fatal("agent output never logged at all; the test proves nothing")
	}

	logs := central.text()
	if !strings.Contains(logs, "task accepted") || !strings.Contains(logs, "task_id=task-secret") {
		t.Fatal("central sink received no task records; the test proves nothing")
	}
	events, _ := json.Marshal(h.store.Events("task-secret"))
	var labels strings.Builder
	for _, m := range collect(t, h.reader) {
		for _, set := range attributeSets(m) {
			for _, kv := range set {
				fmt.Fprintf(&labels, "%s=%s\n", kv.Key, kv.Value.Emit())
			}
		}
	}
	for _, n := range needles {
		if strings.Contains(logs, n) {
			t.Errorf("central log sink contains %q", n)
		}
		if bytes.Contains(events, []byte(n)) {
			t.Errorf("task_events payloads contain %q", n)
		}
		if strings.Contains(labels.String(), n) {
			t.Errorf("a metric attribute contains %q", n)
		}
	}
}

func attributeSets(m metricdata.Metrics) [][]attribute.KeyValue {
	var out [][]attribute.KeyValue
	add := func(kvs []attribute.KeyValue) { out = append(out, kvs) }
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes.ToSlice())
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes.ToSlice())
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes.ToSlice())
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes.ToSlice())
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes.ToSlice())
		}
	}
	return out
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
