// Package launcherclient holds the ContainerLauncher interface; its
// production implementation over gRPC + mTLS lands in Phase 3.
package launcherclient

import (
	"context"
	"time"

	"google.golang.org/grpc/status"
)

// LaunchRequest mirrors api/proto/launcher/v1/launcher.proto's LaunchRequest.
type LaunchRequest struct {
	InvocationID   string
	TaskID         string
	StepType       string // "agent-stage" | "deterministic-step"
	StepID         string
	PayloadRef     string // mock: fixture key; real: image+command ref
	Env            map[string]string
	TimeoutSeconds int32
	GrantedScopes  []string
}

// RunResult mirrors api/proto/launcher/v1/launcher.proto's LaunchResponse.
type RunResult struct {
	InvocationID string
	ExitCode     int32
	Status       string // completed | timed_out | killed | error
	StartedAt    time.Time
	FinishedAt   time.Time

	// Outcome is the enumerated LaunchOutcome (Outcome* below); "" when the
	// server sent none (an older launcher). The durations are zero then too.
	Outcome          string
	QueueWait        time.Duration
	Boot             time.Duration
	GuestRun         time.Duration
	EffectiveTimeout time.Duration
}

// Launch outcomes, the lower-cased LaunchOutcome enum names (LA-1).
const (
	OutcomeCompleted   = "completed"
	OutcomeBootFailure = "boot_failure"
	OutcomeTimeout     = "timeout"
	OutcomeGuestError  = "guest_error"
	OutcomeKilled      = "killed"
)

// LaunchError is a failed Launch for which the server still delivered its
// LaunchResponse (as a gRPC error detail): Result and Outcome say how the
// invocation ended. Err is the underlying gRPC error, so status.Code(err)
// and status.FromError keep working (GRPCStatus).
type LaunchError struct {
	Result  RunResult
	Outcome string
	Err     error
}

func (e *LaunchError) Error() string { return e.Err.Error() }
func (e *LaunchError) Unwrap() error { return e.Err }

// GRPCStatus lets status.Code/status.FromError see through the wrapper.
func (e *LaunchError) GRPCStatus() *status.Status { return status.Convert(e.Err) }

// LogBundle mirrors api/proto/launcher/v1/launcher.proto's ExtractLogsResponse.
type LogBundle struct {
	Stdout               []byte
	Stderr               []byte
	StructuredResultJSON []byte
}

// ContainerLauncher is ADR-9/ADR-11's execution-primitive test-seam
// interface. Its production implementation is a gRPC + mTLS client over
// api/proto/launcher/v1/launcher.proto; both AgentStageExecutor and
// DeterministicStepExecutor dispatch exclusively through it (ADR-11's
// unified execution primitive). launcher-mock (Phase 3) and, eventually,
// the real ADR-10 jailer/Firecracker launcher implement its server side.
type ContainerLauncher interface {
	Launch(ctx context.Context, req LaunchRequest) (RunResult, error)
	Kill(ctx context.Context, invocationID string) error
	ExtractLogs(ctx context.Context, invocationID string) (LogBundle, error)
}
