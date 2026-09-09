package executor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/secretsclient"
)

// credentialTTL is a Phase 4 stub constant: real per-step TTL sizing (e.g.
// derived from the step's timeout_seconds) is a future refinement.
const credentialTTL = 10 * time.Minute

// mintCredential issues a scoped credential for one step invocation, as the
// bot account PDP resolved that step to run as.
func mintCredential(ctx context.Context, secrets secretsclient.SecretsClient, sc StepContext) (secretsclient.Credential, error) {
	cred, err := secrets.IssueCredential(ctx, secretsclient.CredentialRequest{
		InvocationID: invocationID(sc),
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
	return env
}
