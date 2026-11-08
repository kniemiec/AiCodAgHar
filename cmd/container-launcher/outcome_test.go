package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry/telemetrytest"
)

// launchDetail returns the LaunchResponse a Launch delivered: the response
// on success, the error detail on failure (design §7). It fails the test
// when a failed Launch carries no detail.
func launchDetail(t *testing.T, resp *launcherv1.LaunchResponse, err error) *launcherv1.LaunchResponse {
	t.Helper()
	if err == nil {
		return resp
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	for _, d := range status.Convert(err).Details() {
		if r, ok := d.(*launcherv1.LaunchResponse); ok {
			return r
		}
	}
	t.Fatalf("failed Launch carried no LaunchResponse detail: %v", err)
	return nil
}

// TestLaunch_Outcomes is LA-1: one case per LaunchOutcome, with boot vs guest
// error told apart by whether the guest was reached, and boot/run durations
// reported on failure as well as success.
func TestLaunch_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		outcome bootOutcome
		err     error
		want    launcherv1.LaunchOutcome
	}{
		{"completed", bootOutcome{status: "completed", reachedGuest: true, boot: time.Second, run: 2 * time.Second}, nil,
			launcherv1.LaunchOutcome_LAUNCH_OUTCOME_COMPLETED},
		{"boot failure", bootOutcome{status: "error", boot: time.Second}, errors.New("jailer died"),
			launcherv1.LaunchOutcome_LAUNCH_OUTCOME_BOOT_FAILURE},
		{"guest error", bootOutcome{status: "error", exitCode: 1, reachedGuest: true, boot: time.Second, run: 2 * time.Second}, errors.New("no result"),
			launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR},
		{"timeout", bootOutcome{status: "timed_out", reachedGuest: true, boot: time.Second, run: 2 * time.Second}, errors.New("timed out"),
			launcherv1.LaunchOutcome_LAUNCH_OUTCOME_TIMEOUT},
		{"killed while running", bootOutcome{status: "killed", reachedGuest: true, boot: time.Second, run: 2 * time.Second}, errInvocationKilled,
			launcherv1.LaunchOutcome_LAUNCH_OUTCOME_KILLED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := telemetrytest.New()
			srv := newTestServer(&fakeBooter{outcome: tc.outcome, err: tc.err})
			srv.metrics = telemetry.NewLauncher(reader.Provider, srv.waiting.Load)

			// Bare-boot step type: no payload_ref to render; its metric label is bounded to "other".
			resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-1", StepType: "bare"})
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("Launch err = %v, want error %v", err, tc.err != nil)
			}
			got := launchDetail(t, resp, err)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", got.Outcome, tc.want)
			}
			if got.Status != tc.outcome.status {
				t.Fatalf("legacy status = %q, want %q", got.Status, tc.outcome.status)
			}
			if got.Boot.AsDuration() != tc.outcome.boot || got.GuestRun.AsDuration() != tc.outcome.run {
				t.Fatalf("boot/run = %v/%v, want %v/%v", got.Boot.AsDuration(), got.GuestRun.AsDuration(), tc.outcome.boot, tc.outcome.run)
			}
			if got.QueueWait == nil {
				t.Fatal("queue_wait not set")
			}
			if n := reader.Value(t, "launcher_invocations_total", map[string]string{"outcome": outcomeLabel(tc.want), "step_type": telemetry.Other}); n != 1 {
				t.Fatalf("launcher_invocations_total{%s} = %v, want 1", outcomeLabel(tc.want), n)
			}
			wantVM := (tc.outcome.boot + tc.outcome.run).Seconds()
			if v := reader.Value(t, "launcher_vm_seconds_total", nil); v != wantVM {
				t.Fatalf("launcher_vm_seconds_total = %v, want %v", v, wantVM)
			}
		})
	}
}

// TestLaunch_KilledWhileQueued: a Kill before the VM slot frees is KILLED with
// zero boot/run, delivered as an error detail.
func TestLaunch_KilledWhileQueued(t *testing.T) {
	booter := &serialBooter{entered: make(chan string, 1), release: make(chan struct{})}
	srv := newTestServer(booter)
	go srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-running"})
	<-booter.entered
	defer func() { booter.release <- struct{}{} }()

	type result struct {
		resp *launcherv1.LaunchResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-queued"})
		ch <- result{resp, err}
	}()
	waitInFlight(t, srv, "inv-queued")
	if _, err := srv.Kill(context.Background(), &launcherv1.KillRequest{InvocationId: "inv-queued"}); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	r := <-ch
	got := launchDetail(t, r.resp, r.err)
	if got.Outcome != launcherv1.LaunchOutcome_LAUNCH_OUTCOME_KILLED {
		t.Fatalf("outcome = %v, want KILLED", got.Outcome)
	}
	if got.Boot.AsDuration() != 0 || got.GuestRun.AsDuration() != 0 {
		t.Fatalf("boot/run = %v/%v, want zero for a queued kill", got.Boot.AsDuration(), got.GuestRun.AsDuration())
	}
}

// TestLaunch_QueueWait: with one VM slot, the second of two concurrent
// launches reports the time it waited behind the first, the first reports
// (almost) none, and launcher_queue_depth shows the waiter meanwhile.
func TestLaunch_QueueWait(t *testing.T) {
	reader := telemetrytest.New()
	booter := &serialBooter{entered: make(chan string, 2), release: make(chan struct{})}
	srv := newTestServer(booter)
	srv.metrics = telemetry.NewLauncher(reader.Provider, srv.waiting.Load)

	type result struct {
		id   string
		resp *launcherv1.LaunchResponse
	}
	ch := make(chan result, 2)
	launch := func(id string) {
		resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: id})
		if err != nil {
			t.Errorf("Launch %s: %v", id, err)
		}
		ch <- result{id, resp}
	}
	go launch("inv-a")
	first := <-booter.entered
	second := "inv-b"
	if first == "inv-b" {
		second = "inv-a"
	}
	go launch(second)
	waitInFlight(t, srv, second)

	if depth := reader.Value(t, "launcher_queue_depth", nil); depth != 1 {
		t.Fatalf("launcher_queue_depth while one waits = %v, want 1", depth)
	}
	const hold = 200 * time.Millisecond
	time.Sleep(hold)
	booter.release <- struct{}{}
	<-booter.entered
	booter.release <- struct{}{}

	waits := map[string]time.Duration{}
	for range 2 {
		r := <-ch
		waits[r.id] = r.resp.QueueWait.AsDuration()
	}
	if waits[second] < hold {
		t.Fatalf("queued launch queue_wait = %v, want >= %v (it waited behind %s)", waits[second], hold, first)
	}
	if waits[first] >= waits[second] {
		t.Fatalf("first launch queue_wait %v not below the queued one's %v", waits[first], waits[second])
	}
	if depth := reader.Value(t, "launcher_queue_depth", nil); depth != 0 {
		t.Fatalf("launcher_queue_depth after both ran = %v, want 0", depth)
	}
}

// TestLaunch_EffectiveTimeout is LA-3: a positive request timeout wins, else
// the launcher default applies, and the response says which.
func TestLaunch_EffectiveTimeout(t *testing.T) {
	for _, tc := range []struct {
		requested int32
		want      int32
	}{{42, 42}, {0, 1800}, {-1, 1800}} {
		srv := newTestServer(&fakeBooter{outcome: bootOutcome{status: "completed"}})
		srv.defaultTimeoutSeconds = 1800
		resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-1", TimeoutSeconds: tc.requested})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if resp.EffectiveTimeoutSeconds != tc.want {
			t.Errorf("requested %d: effective_timeout_seconds = %d, want %d", tc.requested, resp.EffectiveTimeoutSeconds, tc.want)
		}
	}
}

// TestLaunch_RedactionsCounted: the booter's replacement count reaches
// launcher_redactions_total.
func TestLaunch_RedactionsCounted(t *testing.T) {
	reader := telemetrytest.New()
	srv := newTestServer(&fakeBooter{outcome: bootOutcome{status: "completed", reachedGuest: true, redactions: 3}})
	srv.metrics = telemetry.NewLauncher(reader.Provider, srv.waiting.Load)
	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-1", StepType: "weird"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if n := reader.Value(t, "launcher_redactions_total", map[string]string{"step_type": telemetry.Other}); n != 3 {
		t.Fatalf("launcher_redactions_total{step_type=other} = %v, want 3 (unknown step types are bounded)", n)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		o    bootOutcome
		want launcherv1.LaunchOutcome
	}{
		{bootOutcome{status: "completed"}, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_COMPLETED},
		{bootOutcome{status: "timed_out", reachedGuest: true}, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_TIMEOUT},
		{bootOutcome{status: "killed"}, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_KILLED},
		{bootOutcome{status: "killed", reachedGuest: true}, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_KILLED},
		{bootOutcome{status: "error"}, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_BOOT_FAILURE},
		{bootOutcome{status: "error", reachedGuest: true}, launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR},
	}
	for _, tc := range cases {
		if got := classify(tc.o); got != tc.want {
			t.Errorf("classify(%+v) = %v, want %v", tc.o, got, tc.want)
		}
	}
}

func waitInFlight(t *testing.T, srv *launcherServer, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		_, ok := srv.inFlight[id]
		srv.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never registered as in-flight", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
