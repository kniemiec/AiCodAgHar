package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// DeterministicStepExecutor runs a "deterministic-step" step. It dispatches
// exclusively through ContainerLauncher (ADR-11), minting a scoped
// credential first and revoking it after (Phase 4). As a Phase 2 stub it
// treats any non-zero exit code as a finding — it does not yet consult the
// step's real expected_exit_codes list from .harness/stages.yaml (schemas/
// stages.schema.json defaults that list to [0]).
//
// PayloadRef is set to the resolved step's StepID — see AgentStageExecutor's
// doc comment for why.
type DeterministicStepExecutor struct {
	Launcher   launcherclient.ContainerLauncher
	Secrets    secretsclient.SecretsClient
	Audit      audit.AuditWriter
	ForgejoURL string
}

func (e *DeterministicStepExecutor) Execute(ctx context.Context, sc StepContext) (domain.StepRun, error) {
	invocationID := InvocationID(sc)
	ctx = stepContext(ctx, sc)

	cred, err := mintCredential(ctx, e.Secrets, sc)
	if err != nil {
		return domain.StepRun{}, err
	}
	defer revokeCredential(e.Secrets, sc, cred)

	env := launchEnv(e.ForgejoURL, sc, cred)
	slog.DebugContext(ctx, "launch request", "payload_ref", sc.Step.StepID, "env_keys", logging.Keys(env), "granted_scopes", sc.GrantedScopes)
	launchStarted := time.Now()
	result, err := e.Launcher.Launch(ctx, launcherclient.LaunchRequest{
		InvocationID:   invocationID,
		TaskID:         sc.Task.ID,
		StepType:       StepTypeDeterministicStep,
		StepID:         sc.Step.StepID,
		PayloadRef:     sc.Step.StepID,
		Env:            env,
		GrantedScopes:  sc.GrantedScopes,
		TimeoutSeconds: 0,
	})
	if err != nil {
		slog.ErrorContext(ctx, "launch failed", "duration", time.Since(launchStarted), "error", err)
		auditLaunchFailure(e.Audit, sc, invocationID)
		return failedLaunchRun(sc, StepTypeDeterministicStep, invocationID, launchInfo(result, err)),
			classifyLaunchError(fmt.Errorf("launch deterministic-step %s: %w", sc.Step.StepID, err))
	}

	slog.InfoContext(ctx, "launch finished", "status", result.Status, "outcome", result.Outcome, "exit_code", result.ExitCode,
		"started_at", result.StartedAt, "finished_at", result.FinishedAt, "duration", time.Since(launchStarted),
		"queue_wait", result.QueueWait, "boot", result.Boot, "guest_run", result.GuestRun, "effective_timeout", result.EffectiveTimeout)

	logs, err := e.Launcher.ExtractLogs(ctx, result.InvocationID)
	if err != nil {
		return domain.StepRun{}, failure.Dependency(failure.DepLauncher, fmt.Errorf("extract logs for %s: %w", result.InvocationID, err))
	}
	slog.DebugContext(ctx, "logs extracted", "stdout_bytes", len(logs.Stdout), "stderr_bytes", len(logs.Stderr),
		"structured_result", logging.Truncate(string(logs.StructuredResultJSON), 2048))

	if err := auditStepRun(ctx, e.Audit, sc, invocationID, result); err != nil {
		return domain.StepRun{}, fmt.Errorf("audit deterministic-step %s: %w", sc.Step.StepID, err)
	}

	return domain.StepRun{
		Round:            sc.Round,
		StepID:           sc.Step.StepID,
		StepType:         StepTypeDeterministicStep,
		InvocationID:     result.InvocationID,
		Launch:           launchInfo(result, nil),
		Blocking:         sc.Step.Blocking,
		ExitCode:         result.ExitCode,
		HasFinding:       result.ExitCode != 0,
		StructuredResult: logs.StructuredResultJSON,
	}, nil
}
