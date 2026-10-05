package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// agentStageResult is the Phase 2 stub contract for an agent-stage step's
// ExtractLogsResponse.StructuredResultJSON. Real review-finding severity
// parsing is out of scope until the real agent-stage payload is defined.
type agentStageResult struct {
	HasBlockingFinding bool `json:"has_blocking_finding"`
}

// AgentStageExecutor runs an "agent-stage" step. It dispatches exclusively
// through ContainerLauncher (ADR-11), minting a scoped credential first and
// revoking it after (Phase 4). As a Phase 2 stub it determines whether the
// step produced a blocking finding from a fixed agentStageResult JSON
// contract in the extracted logs.
//
// PayloadRef is set to the resolved step's StepID: in this mocked
// environment, launcher-mock's fixture registry is keyed by the same step
// IDs .harness/stages.yaml uses. The real launcher will decouple
// payload_ref (an image+command ref) from step_id.
type AgentStageExecutor struct {
	Launcher   launcherclient.ContainerLauncher
	Secrets    secretsclient.SecretsClient
	Audit      audit.AuditWriter
	ForgejoURL string
}

func (e *AgentStageExecutor) Execute(ctx context.Context, sc StepContext) (domain.StepRun, error) {
	invocationID := InvocationID(sc)
	log := stepLogger(sc)

	cred, err := mintCredential(ctx, e.Secrets, sc)
	if err != nil {
		return domain.StepRun{}, err
	}
	defer revokeCredential(e.Secrets, sc, cred)

	env := launchEnv(e.ForgejoURL, sc, cred)
	log.DebugContext(ctx, "launch request", "payload_ref", sc.Step.StepID, "env_keys", logging.Keys(env), "granted_scopes", sc.GrantedScopes)
	launchStarted := time.Now()
	result, err := e.Launcher.Launch(ctx, launcherclient.LaunchRequest{
		InvocationID:   invocationID,
		TaskID:         sc.Task.ID,
		StepType:       StepTypeAgentStage,
		StepID:         sc.Step.StepID,
		PayloadRef:     sc.Step.StepID,
		Env:            env,
		GrantedScopes:  sc.GrantedScopes,
		TimeoutSeconds: 0,
	})
	if err != nil {
		log.ErrorContext(ctx, "launch failed", "duration", time.Since(launchStarted), "error", err)
		auditLaunchFailure(e.Audit, sc, invocationID)
		return domain.StepRun{}, fmt.Errorf("launch agent-stage step %s: %w", sc.Step.StepID, err)
	}

	log.InfoContext(ctx, "launch finished", "status", result.Status, "exit_code", result.ExitCode,
		"started_at", result.StartedAt, "finished_at", result.FinishedAt, "duration", time.Since(launchStarted))

	logs, err := e.Launcher.ExtractLogs(ctx, result.InvocationID)
	if err != nil {
		return domain.StepRun{}, fmt.Errorf("extract logs for %s: %w", result.InvocationID, err)
	}
	log.DebugContext(ctx, "logs extracted", "stdout_bytes", len(logs.Stdout), "stderr_bytes", len(logs.Stderr),
		"structured_result", logging.Truncate(string(logs.StructuredResultJSON), 2048))

	var parsed agentStageResult
	if len(logs.StructuredResultJSON) > 0 {
		if err := json.Unmarshal(logs.StructuredResultJSON, &parsed); err != nil {
			return domain.StepRun{}, fmt.Errorf("parse structured result for %s: %w", result.InvocationID, err)
		}
	}

	if err := auditStepRun(ctx, e.Audit, sc, invocationID, result); err != nil {
		return domain.StepRun{}, fmt.Errorf("audit agent-stage step %s: %w", sc.Step.StepID, err)
	}

	return domain.StepRun{
		Round:            sc.Round,
		StepID:           sc.Step.StepID,
		StepType:         StepTypeAgentStage,
		InvocationID:     result.InvocationID,
		Blocking:         sc.Step.Blocking,
		ExitCode:         result.ExitCode,
		HasFinding:       parsed.HasBlockingFinding,
		StructuredResult: logs.StructuredResultJSON,
	}, nil
}
