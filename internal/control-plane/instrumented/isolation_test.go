package instrumented_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry/telemetrytest"
)

// wedgedCollector accepts TCP connections and never answers — the worst
// case for an exporter (worse than connection refused, which fails fast).
func wedgedCollector(t *testing.T) (endpoint string, accepted *atomic.Int64) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted = &atomic.Int64{}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		lis.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return "http://" + lis.Addr().String(), accepted
}

func runTasks(t *testing.T, w *world, prefix string, n int) time.Duration {
	t.Helper()
	start := time.Now()
	for i := range n {
		w.run(t, domain.Task{ID: fmt.Sprintf("%s-%d", prefix, i), TargetRepo: "acme/widgets"})
	}
	return time.Since(start)
}

// TestTelemetry_WedgedCollectorDoesNotSlowTasks is CC-4 / success criterion
// 10: tasks run with metrics and logs exported to a Collector that accepts
// connections but never answers finish within noise of a no-op baseline, and
// shutdown stays bounded by its timeout.
func TestTelemetry_WedgedCollectorDoesNotSlowTasks(t *testing.T) {
	const tasks = 5
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(logging.NewHandler(&bytes.Buffer{}, "control-plane")))

	baseline := runTasks(t, newWorld(telemetry.Noop().MeterProvider), "base", tasks)

	endpoint, accepted := wedgedCollector(t)
	t.Setenv(telemetry.EndpointEnv, endpoint)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20") // ms: export during the run, not after it
	t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "20")
	tel := telemetry.Setup(context.Background(), "control-plane")
	if len(tel.LogHandlers()) == 0 {
		t.Fatal("telemetry did not enable with an endpoint set; the test proves nothing")
	}
	slog.SetDefault(slog.New(logging.NewHandler(&bytes.Buffer{}, "control-plane", tel.LogHandlers()...)))

	wedged := runTasks(t, newWorld(tel.MeterProvider), "wedged", tasks)

	deadline := time.Now().Add(3 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() == 0 {
		t.Fatal("exporter never connected to the wedged collector; the test proves nothing")
	}

	if limit := 3*baseline + 300*time.Millisecond; wedged > limit {
		t.Errorf("%d tasks took %v with a wedged collector vs %v baseline (limit %v)", tasks, wedged, baseline, limit)
	}

	const flush = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), flush)
	defer cancel()
	start := time.Now()
	_ = tel.Shutdown(ctx) // an error is expected: nothing was delivered
	if took := time.Since(start); took > flush+500*time.Millisecond {
		t.Errorf("Shutdown took %v, want bounded by its %v timeout", took, flush)
	}
}

// TestTelemetry_NoSecretsInCentralSinks is LG-6/SEC-1/SEC-2 and success
// criterion 11: a task run with a known minted token, a known task
// description and a known agent-output marker leaves none of the three in
// any log record offered to the central (OTLP) sink, any task event payload,
// or any metric attribute. Controls prove each channel was exercised and that
// the values really entered the system.
func TestTelemetry_NoSecretsInCentralSinks(t *testing.T) {
	const (
		description = "TOP-SECRET-TASK-DESCRIPTION-7731"
		agentOutput = "AGENT-OUTPUT-MARKER"
	)
	var local, central bytes.Buffer
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "json")
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	centralSink := slog.NewJSONHandler(&central, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(logging.NewHandler(&local, "control-plane", centralSink)))

	reader := telemetrytest.New()
	w := newWorld(reader.Provider)
	w.run(t, domain.Task{ID: "task-secret", TargetRepo: "acme/widgets", Description: description})

	// Controls: the values entered the system and every channel carried data.
	launches := w.launcher.Launches()
	if len(launches) == 0 {
		t.Fatal("nothing launched; the test proves nothing")
	}
	token := launches[0].Env["FORGEJO_TOKEN"]
	if token == "" || launches[0].Env["TASK_DESCRIPTION"] != description {
		t.Fatalf("token/description never reached the launcher (env keys %v); the test proves nothing", logging.Keys(launches[0].Env))
	}
	if !strings.Contains(local.String(), agentOutput) {
		t.Fatal("agent output never reached even the local stderr sink; the test proves nothing")
	}
	if strings.Count(central.String(), "\n") < 5 {
		t.Fatalf("central sink received %d records; the test proves nothing", strings.Count(central.String(), "\n"))
	}
	events := w.store.Events("task-secret")
	if len(events) < 5 {
		t.Fatalf("only %d task events; the test proves nothing", len(events))
	}
	rm := reader.Collect(t)
	if len(rm.ScopeMetrics) == 0 {
		t.Fatal("no metrics recorded; the test proves nothing")
	}

	forbidden := map[string]string{"minted token": token, "task description": description, "agent output": agentOutput}
	for what, v := range forbidden {
		for _, line := range strings.Split(central.String(), "\n") {
			if strings.Contains(line, v) {
				t.Errorf("central log sink contains the %s: %s", what, line)
			}
		}
		for _, e := range events {
			b, _ := json.Marshal(e)
			if strings.Contains(string(b), v) {
				t.Errorf("%s event payload contains the %s", e.Type, what)
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, p := range reader.Points(t, m.Name) {
					for k, av := range p.Attrs {
						if strings.Contains(av, v) {
							t.Errorf("metric %s label %s contains the %s", m.Name, k, what)
						}
					}
				}
			}
		}
	}
}
