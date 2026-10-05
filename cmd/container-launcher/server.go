package main

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/prompts"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// errInvocationKilled is the context.Cause set on an in-flight invocation's
// context when Kill actually pre-empts it — the launcher's own way to tell
// "Kill was called" apart from any other ctx cancellation (design §6:
// RunResult.Status = "killed" specifically means this, not a generic
// "error").
var errInvocationKilled = errors.New("container-launcher: invocation killed via Kill RPC")

// stepTypeAgentStage and stepTypeDeterministicStep mirror
// internal/control-plane/executor's StepTypeAgentStage/
// StepTypeDeterministicStep constants — duplicated rather than imported,
// since this binary and the control-plane are deliberately separate
// components (design §3) that only share the wire contract
// (LaunchRequest.StepType), not Go packages.
const (
	stepTypeAgentStage        = "agent-stage"
	stepTypeDeterministicStep = "deterministic-step"
)

// vmLauncher boots one invocation's guest and reports how it went. The
// production implementation is *firecrackerLauncher (launch.go); tests
// inject a fake to exercise Launch's RPC-level bookkeeping without a real
// jailer/KVM host underneath (design §14's unit-test layer). env is
// LaunchRequest.Env, forwarded verbatim for real MMDS credential/task
// injection (design §7) — the launcher itself has no opinion on its
// contents. task and command are mutually exclusive: task is the
// already-rendered OpenHands prompt (agent-stage), command is the resolved
// fixed guest shell command (deterministic-step) — both empty for any step
// type prompts.Render/RenderCommand doesn't cover (see Launch below).
// timeoutSeconds is LaunchRequest.TimeoutSeconds, forwarded raw; resolving
// it against the launcher's own configured ceiling (design §8) is the
// implementation's job (resolveTimeout, launch.go), not this interface's.
// ctx is expected to be a context.WithCancelCause-derived context whose
// cause is set to errInvocationKilled specifically when Kill pre-empts it
// — that's how an implementation tells "killed" apart from any other
// cancellation (design §6).
type vmLauncher interface {
	Launch(ctx context.Context, invocationID string, env map[string]string, task, command string, timeoutSeconds int32) (bootOutcome, error)
}

// bootOutcome is a launch attempt's result, in RunResult.Status terms
// (including "killed", design §6), plus whatever ExtractLogs should return
// for this invocation.
type bootOutcome struct {
	exitCode             int32
	status               string
	structuredResultJSON []byte
	stdout               []byte
}

type invocationRecord struct {
	response *launcherv1.LaunchResponse
	logs     *launcherv1.ExtractLogsResponse
}

// launcherServer implements ContainerLauncherServiceServer. Through step 6
// of the design's implementation sequence (§16): an agent-stage Launch call
// renders a real OpenHands prompt (§5) and a deterministic-step call
// resolves a real fixed guest command (§5); either way it boots one real
// jailed Firecracker microVM with that value injected via MMDS (§7) and
// waits for the guest wrapper's vsock event stream to report a completion
// record (§6). Any other/future step type prompts.Render/RenderCommand
// doesn't cover still gets step 1/2's bare boot.
//
// Launch's own gRPC call still only returns once the invocation is done
// (same as mocks/launcher-mock), but it's no longer true that "nothing is
// left in flight by the time Kill could be called": gRPC serves each RPC
// on its own goroutine, so a concurrent Kill call — from a watchdog, an
// operator, or (once the orchestrator grows one) a client-side deadline —
// can now reach a still-running Launch and actually pre-empt it (design
// §8). inFlight tracks that per-invocation cancel hook; invocations tracks
// finished ones for ExtractLogs/duplicate-Kill lookups, same as before.
type launcherServer struct {
	launcherv1.UnimplementedContainerLauncherServiceServer

	booter vmLauncher

	// vmSlot serializes VM boots: every microVM shares the one tap device,
	// guest CID and MAC (egress.DefaultConfig, launch.go), so a second
	// concurrent boot fails with EBUSY on tap0. Launch holds the slot for
	// the whole invocation; callers that fan out (coreloop.fanOut) queue
	// here instead. Waiting is cancellable by Kill.
	vmSlot chan struct{}

	mu          sync.Mutex
	invocations map[string]invocationRecord
	inFlight    map[string]context.CancelCauseFunc
}

func newLauncherServer(cfg config) *launcherServer {
	return &launcherServer{
		booter:      newFirecrackerLauncher(cfg),
		vmSlot:      make(chan struct{}, 1),
		invocations: map[string]invocationRecord{},
		inFlight:    map[string]context.CancelCauseFunc{},
	}
}

// validInvocationID keeps InvocationId safe to embed directly into
// filesystem paths and jailer's own --id argument — it becomes part of the
// jail directory path (internal/launcher/jailer.Config.JailRoot).
var validInvocationID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *launcherServer) Launch(ctx context.Context, req *launcherv1.LaunchRequest) (*launcherv1.LaunchResponse, error) {
	log := slog.With("invocation_id", req.InvocationId, "task_id", req.TaskId, "step_id", req.StepId,
		"step_type", req.StepType, "payload_ref", req.PayloadRef)
	if !validInvocationID.MatchString(req.InvocationId) {
		log.WarnContext(ctx, "launch rejected: invalid invocation_id", "allowed_pattern", validInvocationID.String())
		return nil, status.Errorf(codes.InvalidArgument, "container-launcher: invalid invocation_id %q", req.InvocationId)
	}

	var task, command string
	switch req.StepType {
	case stepTypeAgentStage:
		rendered, ok, err := prompts.Render(req.PayloadRef, req.Env)
		if err != nil {
			log.ErrorContext(ctx, "launch rejected: prompt render failed", "error", err)
			return nil, status.Errorf(codes.Internal, "container-launcher: render prompt for %s: %v", req.PayloadRef, err)
		}
		if !ok {
			log.WarnContext(ctx, "launch rejected: unknown agent-stage payload_ref")
			return nil, status.Errorf(codes.InvalidArgument, "container-launcher: unknown payload_ref %q", req.PayloadRef)
		}
		task = rendered
		log.DebugContext(ctx, "prompt rendered", "prompt_bytes", len(task), "prompt_prefix", logging.Truncate(task, 500))
	case stepTypeDeterministicStep:
		rendered, ok := prompts.RenderCommand(req.PayloadRef)
		if !ok {
			log.WarnContext(ctx, "launch rejected: unknown deterministic-step payload_ref")
			return nil, status.Errorf(codes.InvalidArgument, "container-launcher: unknown payload_ref %q", req.PayloadRef)
		}
		command = rendered
		log.DebugContext(ctx, "command resolved", "command", command)
	default:
		log.WarnContext(ctx, "unrecognized step_type; falling back to bare boot")
	}

	killCtx, cancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	s.inFlight[req.InvocationId] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, req.InvocationId)
		s.mu.Unlock()
		cancel(nil) // release killCtx's resources; no-op on the cause if Kill already set one
	}()

	log.InfoContext(ctx, "launch accepted", "env_keys", logging.Keys(req.Env), "timeout_seconds", req.TimeoutSeconds)
	startedAt := time.Now()
	var outcome bootOutcome
	var bootErr error
	select {
	case s.vmSlot <- struct{}{}:
		if waited := time.Since(startedAt); waited > time.Second {
			log.InfoContext(ctx, "vm slot acquired", "waited", waited)
		}
		outcome, bootErr = s.booter.Launch(killCtx, req.InvocationId, req.Env, task, command, req.TimeoutSeconds)
		<-s.vmSlot
	case <-killCtx.Done():
		outcome, bootErr = bootOutcome{status: statusForCancellation(killCtx)}, context.Cause(killCtx)
	}
	finishedAt := time.Now()

	resp := &launcherv1.LaunchResponse{
		InvocationId: req.InvocationId,
		ExitCode:     outcome.exitCode,
		Status:       outcome.status,
		StartedAt:    timestamppb.New(startedAt),
		FinishedAt:   timestamppb.New(finishedAt),
	}

	s.mu.Lock()
	s.invocations[req.InvocationId] = invocationRecord{
		response: resp,
		logs: &launcherv1.ExtractLogsResponse{
			Stdout:               outcome.stdout,
			StructuredResultJson: outcome.structuredResultJSON,
		},
	}
	s.mu.Unlock()

	attrs := []any{"status", outcome.status, "exit_code", outcome.exitCode, "duration", finishedAt.Sub(startedAt),
		"stdout_bytes", len(outcome.stdout), "structured_result_bytes", len(outcome.structuredResultJSON)}
	if bootErr != nil {
		log.ErrorContext(ctx, "launch failed", append(attrs, "error", bootErr)...)
		if len(outcome.stdout) > 0 {
			log.WarnContext(ctx, "guest event log tail", "tail", eventLogTail(outcome.stdout, eventLogTailBytes))
		}
		return resp, status.Errorf(codes.Internal, "container-launcher: launch %s: %v", req.InvocationId, bootErr)
	}
	log.InfoContext(ctx, "launch finished", attrs...)
	log.DebugContext(ctx, "structured result", "structured_result", logging.Truncate(string(outcome.structuredResultJSON), 2048))
	return resp, nil
}

// eventLogTailBytes is how much of a failed invocation's event log
// Launch logs. ExtractLogs still returns the whole thing, but only while
// this process runs, and the control-plane doesn't fetch logs for a launch
// that returned an error, so the log line is often the only copy.
const eventLogTailBytes = 4096

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// eventLogTail returns the last n bytes of an (already redacted) event log
// with ANSI color codes stripped. OpenHands' rich console output is full
// of them, and they make the log line unreadable.
func eventLogTail(stdout []byte, n int) string {
	clean := ansiEscape.ReplaceAll(stdout, nil)
	if len(clean) > n {
		clean = clean[len(clean)-n:]
	}
	return string(clean)
}

func (s *launcherServer) Kill(ctx context.Context, req *launcherv1.KillRequest) (*launcherv1.KillResponse, error) {
	s.mu.Lock()
	cancel, inFlight := s.inFlight[req.InvocationId]
	_, completed := s.invocations[req.InvocationId]
	s.mu.Unlock()

	if !inFlight && !completed {
		return nil, status.Errorf(codes.NotFound, "container-launcher: unknown invocation %q", req.InvocationId)
	}
	if !inFlight {
		// Known, but already finished by the time Kill arrived — nothing
		// left to pre-empt. Not an error: this is a legitimate race, not a
		// caller mistake.
		slog.InfoContext(ctx, "kill requested for already-finished invocation", "invocation_id", req.InvocationId)
		return &launcherv1.KillResponse{Killed: false}, nil
	}

	slog.InfoContext(ctx, "killing in-flight invocation", "invocation_id", req.InvocationId)
	cancel(errInvocationKilled)
	return &launcherv1.KillResponse{Killed: true}, nil
}

func (s *launcherServer) ExtractLogs(ctx context.Context, req *launcherv1.ExtractLogsRequest) (*launcherv1.ExtractLogsResponse, error) {
	s.mu.Lock()
	rec, ok := s.invocations[req.InvocationId]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container-launcher: unknown invocation %q", req.InvocationId)
	}
	return rec.logs, nil
}
