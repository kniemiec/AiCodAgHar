package main

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
)

type invocationRecord struct {
	response *launcherv1.LaunchResponse
	logs     *launcherv1.ExtractLogsResponse
}

// launcherServer implements ContainerLauncherServiceServer. Every Launch
// call runs synchronously against launcher-mock's closed set of fixtures
// (plan Decision 6) — there is no real sandbox, so there is nothing
// asynchronous for Kill to interrupt.
type launcherServer struct {
	launcherv1.UnimplementedContainerLauncherServiceServer

	mu          sync.Mutex
	invocations map[string]invocationRecord
}

func newLauncherServer() *launcherServer {
	return &launcherServer{invocations: map[string]invocationRecord{}}
}

func (s *launcherServer) Launch(ctx context.Context, req *launcherv1.LaunchRequest) (*launcherv1.LaunchResponse, error) {
	fx, ok := fixtures[req.PayloadRef]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "launcher-mock: unknown payload_ref %q", req.PayloadRef)
	}

	startedAt := time.Now()
	exitCode, structuredResult, err := fx(ctx, req)
	finishedAt := time.Now()

	// Outcome and timings mirror the real launcher's contract (design §7,
	// LA-4): there is no queue or VM here, so the durations are zero, and
	// the effective timeout is the caller's (the mock enforces none).
	resp := &launcherv1.LaunchResponse{
		InvocationId:            req.InvocationId,
		ExitCode:                exitCode,
		Status:                  "completed",
		StartedAt:               timestamppb.New(startedAt),
		FinishedAt:              timestamppb.New(finishedAt),
		Outcome:                 launcherv1.LaunchOutcome_LAUNCH_OUTCOME_COMPLETED,
		QueueWait:               durationpb.New(0),
		Boot:                    durationpb.New(0),
		GuestRun:                durationpb.New(0),
		EffectiveTimeoutSeconds: req.TimeoutSeconds,
	}
	if err != nil {
		resp.Status = "error"
		resp.Outcome = launcherv1.LaunchOutcome_LAUNCH_OUTCOME_GUEST_ERROR
	}

	s.mu.Lock()
	s.invocations[req.InvocationId] = invocationRecord{
		response: resp,
		logs: &launcherv1.ExtractLogsResponse{
			StructuredResultJson: structuredResult,
		},
	}
	s.mu.Unlock()

	if err != nil {
		// Like the real launcher: the response rides along as a status
		// detail, since gRPC sends no body with a non-OK status.
		st := status.Newf(codes.Internal, "launcher-mock: fixture %q failed: %v", req.PayloadRef, err)
		if withDetails, derr := st.WithDetails(resp); derr == nil {
			st = withDetails
		}
		return resp, st.Err()
	}
	return resp, nil
}

func (s *launcherServer) Kill(ctx context.Context, req *launcherv1.KillRequest) (*launcherv1.KillResponse, error) {
	s.mu.Lock()
	_, ok := s.invocations[req.InvocationId]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "launcher-mock: unknown invocation %q", req.InvocationId)
	}
	// Launch is synchronous in this mock: by the time Kill can be called,
	// there is nothing in flight left to kill.
	return &launcherv1.KillResponse{Killed: false}, nil
}

func (s *launcherServer) ExtractLogs(ctx context.Context, req *launcherv1.ExtractLogsRequest) (*launcherv1.ExtractLogsResponse, error) {
	s.mu.Lock()
	rec, ok := s.invocations[req.InvocationId]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "launcher-mock: unknown invocation %q", req.InvocationId)
	}
	return rec.logs, nil
}
