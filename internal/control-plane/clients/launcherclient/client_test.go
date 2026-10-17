package launcherclient

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
)

func TestLaunchResponseDetail(t *testing.T) {
	resp := &launcherv1.LaunchResponse{
		InvocationId: "inv-1", Status: "error", ExitCode: 1,
		Outcome:   launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR,
		QueueWait: durationpb.New(2 * time.Second), Boot: durationpb.New(3 * time.Second), GuestRun: durationpb.New(4 * time.Second),
		EffectiveTimeoutSeconds: 1800,
	}
	st, err := status.New(codes.Internal, "launch failed").WithDetails(resp)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := fmt.Errorf("launcherclient: launch inv-1: %w", st.Err())

	detail := launchResponseDetail(wrapped)
	if detail == nil {
		t.Fatal("detail not found through the wrapped error")
	}
	got := runResult(detail)
	want := RunResult{InvocationID: "inv-1", Status: "error", ExitCode: 1, Outcome: OutcomeGuestError,
		QueueWait: 2 * time.Second, Boot: 3 * time.Second, GuestRun: 4 * time.Second, EffectiveTimeout: 1800 * time.Second}
	if got != want {
		t.Errorf("runResult = %+v, want %+v", got, want)
	}

	le := &LaunchError{Result: got, Outcome: got.Outcome, Err: wrapped}
	if status.Code(le) != codes.Internal {
		t.Errorf("status.Code through LaunchError = %v, want Internal", status.Code(le))
	}
	var target *LaunchError
	if !errors.As(fmt.Errorf("outer: %w", le), &target) || target.Outcome != OutcomeGuestError {
		t.Error("LaunchError not recoverable with errors.As")
	}
}

// An old launcher sends no detail: callers fall back to the gRPC code.
func TestLaunchResponseDetail_OldServer(t *testing.T) {
	if d := launchResponseDetail(status.Error(codes.Internal, "boom")); d != nil {
		t.Errorf("detail = %v, want nil", d)
	}
	if d := launchResponseDetail(errors.New("not a status")); d != nil {
		t.Errorf("detail = %v, want nil", d)
	}
	// An old response (no outcome fields) converts to zero values.
	if r := runResult(&launcherv1.LaunchResponse{Status: "completed"}); r.Outcome != "" || r.QueueWait != 0 || r.EffectiveTimeout != 0 {
		t.Errorf("old response converted to %+v", r)
	}
}
