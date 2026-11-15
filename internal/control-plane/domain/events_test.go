package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
)

func TestStepResolvedEvent_LauncherFields(t *testing.T) {
	run := StepRun{Launch: LaunchInfo{Outcome: "guest_error", QueueWait: 1500 * time.Millisecond, Boot: 2 * time.Second,
		GuestRun: 3 * time.Second, EffectiveTimeout: 1800 * time.Second}}
	e := StepResolvedEvent("t", 1, "review", "agent-stage", "inv", run, failure.Agent(errors.New("bad")), 7*time.Second)
	want := map[string]any{
		"outcome": StepRunStatusFailed, "error_class": "agent", "step_type": "agent-stage", "duration_ms": int64(7000),
		"launcher_outcome": "guest_error", "queue_wait_ms": int64(1500), "boot_ms": int64(2000), "run_ms": int64(3000),
		"effective_timeout_s": int64(1800),
	}
	for k, v := range want {
		if e.Payload[k] != v {
			t.Errorf("payload[%s] = %v (%T), want %v", k, e.Payload[k], e.Payload[k], v)
		}
	}

	plain := StepResolvedEvent("t", 1, "review", "agent-stage", "inv", StepRun{}, nil, time.Second)
	if _, has := plain.Payload["launcher_outcome"]; has {
		t.Error("launcher fields present without a launcher outcome")
	}
}
