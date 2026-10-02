package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
)

type fakeBooter struct {
	outcome           bootOutcome
	err               error
	gotID             string
	gotEnv            map[string]string
	gotTask           string
	gotCommand        string
	gotTimeoutSeconds int32

	// started, when non-nil, is closed as soon as Launch is entered — lets
	// a test wait for the RPC to actually reach the booter (and hence for
	// server.go to have registered it as in-flight) before acting further.
	started chan struct{}
	// block, when non-nil, is waited on (or ctx.Done()) before Launch
	// returns — used together with started to simulate an in-flight
	// invocation for Kill tests.
	block <-chan struct{}
}

func (f *fakeBooter) Launch(ctx context.Context, invocationID string, env map[string]string, task, command string, timeoutSeconds int32) (bootOutcome, error) {
	f.gotID = invocationID
	f.gotEnv = env
	f.gotTask = task
	f.gotCommand = command
	f.gotTimeoutSeconds = timeoutSeconds
	if f.started != nil {
		close(f.started)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return bootOutcome{status: statusForCancellation(ctx)}, context.Cause(ctx)
		}
	}
	return f.outcome, f.err
}

func newTestServer(booter vmLauncher) *launcherServer {
	return &launcherServer{booter: booter, invocations: map[string]invocationRecord{}, inFlight: map[string]context.CancelCauseFunc{}}
}

func TestLaunch_Success(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 0, status: "completed"}}
	srv := newTestServer(booter)

	resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if resp.Status != "completed" || resp.ExitCode != 0 {
		t.Errorf("resp = %+v", resp)
	}
	if booter.gotID != "inv-1" {
		t.Errorf("booter got invocation id %q, want inv-1", booter.gotID)
	}
}

func TestLaunch_ForwardsEnv(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 0, status: "completed"}}
	srv := newTestServer(booter)

	env := map[string]string{"FORGEJO_TOKEN": "secret-token", "TARGET_REPO": "org/repo"}
	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-5", Env: env}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if booter.gotEnv["FORGEJO_TOKEN"] != "secret-token" || booter.gotEnv["TARGET_REPO"] != "org/repo" {
		t.Errorf("booter got env %+v, want %+v", booter.gotEnv, env)
	}
}

func TestLaunch_AgentStage_RendersAndForwardsTask(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 0, status: "completed"}}
	srv := newTestServer(booter)

	env := map[string]string{"TARGET_REPO": "org/repo", "BRANCH": "add-feature"}
	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{
		InvocationId: "inv-6",
		StepType:     stepTypeAgentStage,
		PayloadRef:   "implement",
		Env:          env,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if booter.gotTask == "" {
		t.Error("booter got empty task, want a rendered prompt")
	}
}

func TestLaunch_AgentStage_UnknownPayloadRef(t *testing.T) {
	srv := newTestServer(&fakeBooter{})

	_, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{
		InvocationId: "inv-7",
		StepType:     stepTypeAgentStage,
		PayloadRef:   "no-such-step",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestLaunch_DeterministicStep_RendersAndForwardsCommand(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 0, status: "completed"}}
	srv := newTestServer(booter)

	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{
		InvocationId: "inv-8",
		StepType:     stepTypeDeterministicStep,
		PayloadRef:   "unit-tests",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if booter.gotCommand == "" {
		t.Error("booter got empty command, want a resolved fixed guest command")
	}
	if booter.gotTask != "" {
		t.Errorf("booter got task %q, want empty for a deterministic-step request", booter.gotTask)
	}
}

func TestLaunch_DeterministicStep_UnknownPayloadRef(t *testing.T) {
	srv := newTestServer(&fakeBooter{})

	_, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{
		InvocationId: "inv-9",
		StepType:     stepTypeDeterministicStep,
		PayloadRef:   "no-such-step",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestLaunch_ForwardsTimeoutSeconds(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 0, status: "completed"}}
	srv := newTestServer(booter)

	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-10", TimeoutSeconds: 42}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if booter.gotTimeoutSeconds != 42 {
		t.Errorf("booter got timeoutSeconds %d, want 42", booter.gotTimeoutSeconds)
	}
}

func TestLaunch_BootError(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 1, status: "error"}, err: errors.New("boom")}
	srv := newTestServer(booter)

	resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-2"})
	if err == nil {
		t.Fatal("expected error")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
	if resp.Status != "error" {
		t.Errorf("resp.Status = %q, want error", resp.Status)
	}
}

func TestLaunch_InvalidInvocationID(t *testing.T) {
	srv := newTestServer(&fakeBooter{})

	_, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "../etc"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestKill_NotFound(t *testing.T) {
	srv := newTestServer(&fakeBooter{})

	_, err := srv.Kill(context.Background(), &launcherv1.KillRequest{InvocationId: "unknown"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestKill_AfterLaunch(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{exitCode: 0, status: "completed"}}
	srv := newTestServer(booter)

	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-3"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	resp, err := srv.Kill(context.Background(), &launcherv1.KillRequest{InvocationId: "inv-3"})
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if resp.Killed {
		t.Errorf("resp.Killed = true, want false (the invocation already finished by the time Kill arrived)")
	}
}

func TestKill_PreemptsInFlightInvocation(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{}) // never closed — this Launch only ever returns via Kill's cancellation
	booter := &fakeBooter{started: started, block: block}
	srv := newTestServer(booter)

	respCh := make(chan *launcherv1.LaunchResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-kill"})
		respCh <- resp
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Launch never reached the booter")
	}

	killResp, err := srv.Kill(context.Background(), &launcherv1.KillRequest{InvocationId: "inv-kill"})
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killResp.Killed {
		t.Error("Killed = false, want true for an in-flight invocation")
	}

	select {
	case resp := <-respCh:
		if resp.Status != "killed" {
			t.Errorf("resp.Status = %q, want killed", resp.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Launch never returned after Kill")
	}
	if err := <-errCh; err == nil {
		t.Error("expected a non-nil error from a killed Launch")
	}
}

func TestExtractLogs_AfterLaunch(t *testing.T) {
	booter := &fakeBooter{outcome: bootOutcome{
		exitCode:             0,
		status:               "completed",
		structuredResultJSON: []byte(`{"has_blocking_finding":false}`),
		stdout:               []byte("event log line"),
	}}
	srv := newTestServer(booter)

	if _, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{InvocationId: "inv-4"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	logs, err := srv.ExtractLogs(context.Background(), &launcherv1.ExtractLogsRequest{InvocationId: "inv-4"})
	if err != nil {
		t.Fatalf("ExtractLogs: %v", err)
	}
	if string(logs.StructuredResultJson) != `{"has_blocking_finding":false}` {
		t.Errorf("StructuredResultJson = %q", logs.StructuredResultJson)
	}
	if string(logs.Stdout) != "event log line" {
		t.Errorf("Stdout = %q", logs.Stdout)
	}
}

func TestExtractLogs_NotFound(t *testing.T) {
	srv := newTestServer(&fakeBooter{})

	_, err := srv.ExtractLogs(context.Background(), &launcherv1.ExtractLogsRequest{InvocationId: "unknown"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestEventLogTail_StripsANSIAndKeepsEnd(t *testing.T) {
	got := eventLogTail([]byte("\x1b[92mstart\x1b[0m middle \x1b[1;3mend\x1b[0m"), 9)
	if got != "iddle end" {
		t.Errorf("eventLogTail = %q, want %q", got, "iddle end")
	}
}
