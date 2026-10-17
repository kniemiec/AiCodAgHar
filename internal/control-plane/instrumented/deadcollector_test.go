package instrumented_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/instrumented"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// Success criterion 10 (CC-4): with the Collector unreachable, a task
// completes as fast as with telemetry off — export never blocks the task.
func TestTaskUnaffectedByDeadCollector(t *testing.T) {
	timeTasks := func(t *testing.T, endpoint string) time.Duration {
		t.Setenv(telemetry.EndpointEnv, endpoint)
		tel, err := telemetry.Setup(context.Background(), "control-plane")
		if err != nil {
			t.Fatal(err)
		}
		prev := slog.Default()
		slog.SetDefault(slog.New(logging.NewHandler(io.Discard, "control-plane", tel.LogHandler)))
		defer slog.SetDefault(prev)

		h := newHarnessWith(instrumented.NewMetrics(tel.Meter(), instrumented.Options{}), nil)
		h.policy.EvaluateFunc = policyFor()
		start := time.Now()
		for i := range 20 {
			h.run(t, fmt.Sprintf("task-%d", i), "acme/widgets")
		}
		elapsed := time.Since(start)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = tel.Shutdown(ctx)
		return elapsed
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	l.Close()

	off := timeTasks(t, "")
	withDead := timeTasks(t, dead)
	if withDead > off+time.Second {
		t.Errorf("20 tasks took %v with a dead Collector vs %v with telemetry off", withDead, off)
	}
}
