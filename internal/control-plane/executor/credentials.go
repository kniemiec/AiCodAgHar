package executor

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		return secretsclient.Credential{}, failure.Dependency(failure.DepSecretsIssuer, fmt.Errorf("mint credential for %s as %s: %w", sc.Step.StepID, sc.Step.BotAccount, err))
	}
	stepLogger(sc).DebugContext(ctx, "credential minted", "credential_id", cred.ID, "bot_account", sc.Step.BotAccount, "scopes", sc.GrantedScopes, "ttl", credentialTTL)
	return cred, nil
}

// revokeCredential revokes cred on a fresh context (the step's own ctx may
// already be done), logging rather than returning a failure: revocation is
// best-effort cleanup that must not mask the step's own outcome.
func revokeCredential(secrets secretsclient.SecretsClient, sc StepContext, cred secretsclient.Credential) {
	if err := secrets.RevokeCredential(context.Background(), cred.ID); err != nil {
		stepLogger(sc).Warn("credential revoke failed", "credential_id", cred.ID, "error", err)
	}
}

// classifyLaunchError classes a ContainerLauncher.Launch error by gRPC code
// alone: the launcher's per-outcome detail (BOOT_FAILURE vs GUEST_ERROR vs
// TIMEOUT) arrives with the launcher outcome contract (design §7) and will
// refine this. Until then Unavailable/DeadlineExceeded/Canceled mean the
// launcher itself was unreachable (dependency), InvalidArgument is our own
// bad request (platform), and anything else the launcher reports — in
// practice a failed boot — is an infrastructure fault.
func classifyLaunchError(err error) error {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return failure.Dependency(failure.DepLauncher, err)
	case codes.InvalidArgument:
		return failure.Platform(err)
	}
	return failure.Infrastructure(err)
}

// stepLogger carries the correlation attributes every executor log line needs.
func stepLogger(sc StepContext) *slog.Logger {
	return slog.With("task_id", sc.Task.ID, "round", sc.Round, "step_id", sc.Step.StepID, "invocation_id", InvocationID(sc))
}

// auditLaunchFailure writes the failed-launch audit row, logging rather than
// returning its own error so the launch error stays the one reported.
func auditLaunchFailure(w audit.AuditWriter, sc StepContext, invocationID string) {
	if err := auditStepRun(context.Background(), w, sc, invocationID, launcherclient.RunResult{InvocationID: invocationID, Status: "error"}); err != nil {
		stepLogger(sc).Warn("audit of failed launch failed", "error", err)
	}
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
	if sc.Task.BaseBranch != "" {
		env["BASE_BRANCH"] = sc.Task.BaseBranch
	}
	if sc.Task.PRNumber != 0 {
		env["PR_NUMBER"] = strconv.Itoa(sc.Task.PRNumber)
	}
	if len(sc.PriorFindings) > 0 {
		env["PRIOR_FINDINGS_JSON"] = string(sc.PriorFindings)
	}
	if sc.Task.Description != "" {
		env["TASK_DESCRIPTION"] = sc.Task.Description
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
