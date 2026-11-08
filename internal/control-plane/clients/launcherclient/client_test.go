package launcherclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
)

// stubServer answers Launch with launch's result.
type stubServer struct {
	launcherv1.UnimplementedContainerLauncherServiceServer
	launch func() (*launcherv1.LaunchResponse, error)
}

func (s stubServer) Launch(context.Context, *launcherv1.LaunchRequest) (*launcherv1.LaunchResponse, error) {
	return s.launch()
}

// newBufClient serves stub over an in-memory listener and returns a Client
// dialled to it (no TLS: the wire contract is what's under test).
func newBufClient(t *testing.T, stub stubServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	launcherv1.RegisterContainerLauncherServiceServer(srv, stub)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &Client{conn: conn, rpc: launcherv1.NewContainerLauncherServiceClient(conn)}
}

func timedResponse(outcome launcherv1.LaunchOutcome, status string) *launcherv1.LaunchResponse {
	return &launcherv1.LaunchResponse{
		InvocationId: "inv-1", Status: status, Outcome: outcome,
		QueueWait: durationpb.New(time.Second), Boot: durationpb.New(2 * time.Second), GuestRun: durationpb.New(3 * time.Second),
		EffectiveTimeoutSeconds: 1800,
	}
}

func TestLaunch_SuccessCarriesOutcomeAndTimings(t *testing.T) {
	c := newBufClient(t, stubServer{launch: func() (*launcherv1.LaunchResponse, error) {
		return timedResponse(launcherv1.LaunchOutcome_LAUNCH_OUTCOME_COMPLETED, "completed"), nil
	}})
	res, err := c.Launch(context.Background(), LaunchRequest{InvocationID: "inv-1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.Outcome != OutcomeCompleted || res.QueueWait != time.Second || res.Boot != 2*time.Second ||
		res.GuestRun != 3*time.Second || res.EffectiveTimeout != 1800*time.Second {
		t.Fatalf("result = %+v", res)
	}
}

// TestLaunch_FailureDetailRoundTrip: a failed Launch whose status carries the
// LaunchResponse comes back as *LaunchError with the outcome and timings,
// and the gRPC code still reads through it.
func TestLaunch_FailureDetailRoundTrip(t *testing.T) {
	c := newBufClient(t, stubServer{launch: func() (*launcherv1.LaunchResponse, error) {
		st, err := status.New(codes.Internal, "guest error").
			WithDetails(timedResponse(launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR, "error"))
		if err != nil {
			return nil, err
		}
		return nil, st.Err()
	}})
	res, err := c.Launch(context.Background(), LaunchRequest{InvocationID: "inv-1"})
	var le *LaunchError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v (%T), want *LaunchError", err, err)
	}
	if le.Outcome != OutcomeGuestError || le.Result.Boot != 2*time.Second || le.Result.GuestRun != 3*time.Second {
		t.Fatalf("LaunchError = %+v", le)
	}
	if res.Outcome != OutcomeGuestError {
		t.Fatalf("returned result outcome = %q, want guest_error", res.Outcome)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("status.Code through LaunchError = %v, want Internal", status.Code(err))
	}
}

// TestLaunch_OldServerFallback: a server that predates the outcome contract
// sends no detail (and no outcome on success); the client reports a plain
// error / an empty Outcome, so callers fall back to the gRPC code (CMP-1).
func TestLaunch_OldServerFallback(t *testing.T) {
	c := newBufClient(t, stubServer{launch: func() (*launcherv1.LaunchResponse, error) {
		return nil, status.Error(codes.Internal, "boot failed")
	}})
	_, err := c.Launch(context.Background(), LaunchRequest{InvocationID: "inv-1"})
	var le *LaunchError
	if err == nil || errors.As(err, &le) {
		t.Fatalf("err = %v, want a plain error with no LaunchError", err)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}

	c = newBufClient(t, stubServer{launch: func() (*launcherv1.LaunchResponse, error) {
		return &launcherv1.LaunchResponse{InvocationId: "inv-1", Status: "completed"}, nil
	}})
	res, err := c.Launch(context.Background(), LaunchRequest{InvocationID: "inv-1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.Outcome != "" || res.QueueWait != 0 || res.Boot != 0 || res.Status != "completed" {
		t.Fatalf("old-server result = %+v, want empty outcome and zero durations", res)
	}
}
