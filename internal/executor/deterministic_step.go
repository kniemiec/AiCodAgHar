package executor

import (
	"context"
	"fmt"

	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
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
	ForgejoURL string
}

func (e *DeterministicStepExecutor) Execute(ctx context.Context, sc StepContext) (domain.StepRun, error) {
	invocationID := invocationID(sc)

	cred, err := mintCredential(ctx, e.Secrets, sc)
	if err != nil {
		return domain.StepRun{}, err
	}
	defer func() { _ = e.Secrets.RevokeCredential(context.Background(), cred.ID) }()

	result, err := e.Launcher.Launch(ctx, launcherclient.LaunchRequest{
		InvocationID:   invocationID,
		TaskID:         sc.Task.ID,
		StepType:       StepTypeDeterministicStep,
		StepID:         sc.Step.StepID,
		PayloadRef:     sc.Step.StepID,
		Env:            launchEnv(e.ForgejoURL, sc, cred),
		GrantedScopes:  sc.GrantedScopes,
		TimeoutSeconds: 0,
	})
	if err != nil {
		return domain.StepRun{}, fmt.Errorf("launch deterministic-step %s: %w", sc.Step.StepID, err)
	}

	return domain.StepRun{
		Round:        sc.Round,
		StepID:       sc.Step.StepID,
		StepType:     StepTypeDeterministicStep,
		InvocationID: result.InvocationID,
		Blocking:     sc.Step.Blocking,
		ExitCode:     result.ExitCode,
		HasFinding:   result.ExitCode != 0,
	}, nil
}
