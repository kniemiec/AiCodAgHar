package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
)

// LA-1: one case per outcome, including BOOT_FAILURE vs GUEST_ERROR.
func TestLaunch_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		outcome bootOutcome
		err     error
		want    launcherv1.LaunchOutcome
	}{
		{"completed", bootOutcome{status: "completed", reachedGuest: true}, nil, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_COMPLETED},
		{"boot failure", bootOutcome{status: "error"}, errors.New("jailer"), launcherv1.LaunchOutcome_LAUNCH_OUTCOME_BOOT_FAILURE},
		{"timeout", bootOutcome{status: "timed_out", reachedGuest: true}, errors.New("timeout"), launcherv1.LaunchOutcome_LAUNCH_OUTCOME_TIMEOUT},
		{"guest error", bootOutcome{status: "error", reachedGuest: true}, errors.New("guest"), launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR},
		{"killed", bootOutcome{status: "killed", reachedGuest: true}, errors.New("killed"), launcherv1.LaunchOutcome_LAUNCH_OUTCOME_KILLED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(&fakeBooter{outcome: tc.outcome, err: tc.err})
			resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-outcome"})
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("err = %v, want error %v", err, tc.err != nil)
			}
			if resp.Outcome != tc.want {
				t.Errorf("Outcome = %v, want %v", resp.Outcome, tc.want)
			}
		})
	}
}

func TestLaunch_KilledWhileQueuedIsKilled(t *testing.T) {
	srv := newTestServer(&fakeBooter{})
	srv.vmSlot <- struct{}{} // slot taken: the next Launch queues
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *launcherv1.LaunchResponse, 1)
	go func() {
		resp, _ := srv.Launch(ctx, &launcherv1.LaunchRequest{InvocationId: "inv-queued"})
		done <- resp
	}()
	waitFor(t, func() bool { return srv.waiters.Load() == 1 })
	if _, err := srv.Kill(context.Background(), &launcherv1.KillRequest{InvocationId: "inv-queued"}); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	resp := <-done
	cancel()
	if resp.Outcome != launcherv1.LaunchOutcome_LAUNCH_OUTCOME_KILLED {
		t.Errorf("Outcome = %v, want KILLED", resp.Outcome)
	}
	if resp.Boot.AsDuration() != 0 || resp.GuestRun.AsDuration() != 0 {
		t.Errorf("boot/run = %v/%v, want zero for a never-booted invocation", resp.Boot.AsDuration(), resp.GuestRun.AsDuration())
	}
	if srv.waiters.Load() != 0 {
		t.Errorf("waiters = %d after cancel, want 0", srv.waiters.Load())
	}
}

// The response — outcome and timings — must reach the caller on failure
// too, as a status detail (design §7).
func TestLaunch_FailureCarriesResponseAsDetail(t *testing.T) {
	srv := newTestServer(&fakeBooter{
		outcome: bootOutcome{status: "error", exitCode: 1, reachedGuest: true, boot: 2 * time.Second, run: 3 * time.Second},
		err:     errors.New("guest crashed"),
	})
	srv.defaultTimeoutSeconds = 1800

	_, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-detail"})
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("err = %v, want an Internal status", err)
	}
	var got *launcherv1.LaunchResponse
	for _, d := range st.Details() {
		if r, ok := d.(*launcherv1.LaunchResponse); ok {
			got = r
		}
	}
	if got == nil {
		t.Fatalf("no LaunchResponse detail in %v", st.Details())
	}
	if got.Outcome != launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR || got.ExitCode != 1 || got.Status != "error" {
		t.Errorf("detail = %+v", got)
	}
	if got.Boot.AsDuration() != 2*time.Second || got.GuestRun.AsDuration() != 3*time.Second {
		t.Errorf("boot/run = %v/%v, want 2s/3s", got.Boot.AsDuration(), got.GuestRun.AsDuration())
	}
	if got.EffectiveTimeoutSeconds != 1800 {
		t.Errorf("EffectiveTimeoutSeconds = %d, want the 1800 default", got.EffectiveTimeoutSeconds)
	}
}

// LA-3: a caller's positive timeout wins over the launcher default.
func TestLaunch_EffectiveTimeoutPrecedence(t *testing.T) {
	srv := newTestServer(&fakeBooter{outcome: bootOutcome{status: "completed"}})
	srv.defaultTimeoutSeconds = 1800
	resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-t1", TimeoutSeconds: 42})
	if err != nil {
		t.Fatal(err)
	}
	if resp.EffectiveTimeoutSeconds != 42 {
		t.Errorf("caller timeout: EffectiveTimeoutSeconds = %d, want 42", resp.EffectiveTimeoutSeconds)
	}
	resp, err = srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-t2"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.EffectiveTimeoutSeconds != 1800 {
		t.Errorf("no caller timeout: EffectiveTimeoutSeconds = %d, want 1800", resp.EffectiveTimeoutSeconds)
	}
}

// Queue wait is measured separately from the run: the second of two
// concurrent launches reports roughly how long the first held the slot.
func TestLaunch_QueueWaitMeasured(t *testing.T) {
	booter := &serialBooter{entered: make(chan string, 2), release: make(chan struct{})}
	srv := newTestServer(booter)
	reader := sdkmetric.NewManualReader()
	srv.metrics = newLauncherMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), srv)

	resps := make(chan *launcherv1.LaunchResponse, 2)
	launch := func(id string) {
		resp, _ := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: id})
		resps <- resp
	}
	go launch("inv-q1")
	<-booter.entered
	go launch("inv-q2")
	waitFor(t, func() bool { return srv.waiters.Load() == 1 })

	if v := gaugeValue(t, reader, "launcher_queue_depth"); v != 1 {
		t.Errorf("launcher_queue_depth = %d while one launch is queued, want 1", v)
	}
	const held = 100 * time.Millisecond
	time.Sleep(held)
	booter.release <- struct{}{}
	<-booter.entered
	booter.release <- struct{}{}

	first, second := <-resps, <-resps
	if first.QueueWait.AsDuration() > second.QueueWait.AsDuration() {
		first, second = second, first
	}
	if second.QueueWait.AsDuration() < held {
		t.Errorf("queued launch's QueueWait = %v, want >= %v", second.QueueWait.AsDuration(), held)
	}
	if first.QueueWait.AsDuration() >= held {
		t.Errorf("first launch's QueueWait = %v, want < %v", first.QueueWait.AsDuration(), held)
	}
	if n := counterValue(t, reader, "launcher_invocations_total", "outcome", "completed"); n != 2 {
		t.Errorf("launcher_invocations_total{outcome=completed} = %d, want 2", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func collect(t *testing.T, r *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

func gaugeValue(t *testing.T, r *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	for _, m := range collect(t, r) {
		if g, ok := m.Data.(metricdata.Gauge[int64]); ok && m.Name == name && len(g.DataPoints) > 0 {
			return g.DataPoints[0].Value
		}
	}
	t.Fatalf("gauge %s not found", name)
	return 0
}

func counterValue(t *testing.T, r *sdkmetric.ManualReader, name, key, value string) int64 {
	t.Helper()
	var total int64
	for _, m := range collect(t, r) {
		if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == name {
			for _, dp := range s.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
					total += dp.Value
				}
			}
		}
	}
	return total
}
