package executor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
)

// credentialTTL is a Phase 4 stub constant: real per-step TTL sizing (e.g.
// derived from the step's timeout_seconds) is a future refinement.
const credentialTTL = 10 * time.Minute

// mintCredential issues a scoped credential for one step invocation, as the
// bot account PDP resolved that step to run as.
func mintCredential(ctx context.Context, secrets secretsclient.SecretsClient, sc StepContext) (secretsclient.Credential, error) {
	cred, err := secrets.IssueCredential(ctx, secretsclient.CredentialRequest{
		InvocationID: InvocationID(sc),
		BotAccount:   sc.Step.BotAccount,
		TargetRepo:   sc.Task.TargetRepo,
		Scopes:       sc.GrantedScopes,
		TTL:          credentialTTL,
	})
	if err != nil {
		return secretsclient.Credential{}, fmt.Errorf("mint credential for %s as %s: %w", sc.Step.StepID, sc.Step.BotAccount, err)
	}
	return cred, nil
}

// launchEnv builds the LaunchRequest.env every step needs to reach Forgejo
// as the credential it was just minted: the injected token, target repo,
// and (once the control-plane has opened it) the review PR's branch/number.
func launchEnv(forgejoURL string, sc StepContext, cred secretsclient.Credential) map[string]string {
	env := map[string]string{
		"FORGEJO_URL":   forgejoURL,
		"FORGEJO_TOKEN": cred.Token,
		"TARGET_REPO":   sc.Task.TargetRepo,
	}
	if sc.Task.Branch != "" {
		env["BRANCH"] = sc.Task.Branch
	}
	if sc.Task.PRNumber != 0 {
		env["PR_NUMBER"] = strconv.Itoa(sc.Task.PRNumber)
	}
	if len(sc.PriorFindings) > 0 {
		env["PRIOR_FINDINGS_JSON"] = string(sc.PriorFindings)
	}
	return env
}

// auditStepRun writes this invocation's audit row — the "launch start/stop"
// decision point. Called on both the success and failure launch paths.
func auditStepRun(ctx context.Context, w audit.AuditWriter, sc StepContext, invocationID string, result launcherclient.RunResult) error {
	return w.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
		InvocationID: invocationID,
		TaskID:       sc.Task.ID,
		Round:        sc.Round,
		StepID:       sc.Step.StepID,
		StepType:     sc.Step.Type,
		BotAccount:   sc.Step.BotAccount,
		TargetRepo:   sc.Task.TargetRepo,
		ExitCode:     result.ExitCode,
		Status:       result.Status,
	})
}
