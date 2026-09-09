package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

// fixture is one of launcher-mock's small closed set of payload_ref-keyed
// behaviors (plan Decision 6): each performs a real Forgejo side effect (or
// deliberately none) using the credential injected via LaunchRequest.env,
// proving the credential round-trip and real git-visible outcomes without
// any real sandbox mechanics.
type fixture func(ctx context.Context, req *launcherv1.LaunchRequest) (exitCode int32, structuredResultJSON []byte, err error)

var fixtures = map[string]fixture{
	"noop-success":                  fixtureNoopSuccess,
	"post-pr-comment":               fixturePostPRComment(false),
	"post-blocking-finding-comment": fixturePostPRComment(true),
	"deterministic-fail":            fixtureDeterministicFail,
}

func fixtureNoopSuccess(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	return 0, []byte(`{"has_blocking_finding": false}`), nil
}

func fixtureDeterministicFail(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	return 1, nil, nil
}

// fixturePostPRComment posts a real PR comment using the credential and
// target repo/PR number injected via env — the point being that the
// credential is the one the orchestrator minted for this invocation, not
// launcher-mock's own service credential.
func fixturePostPRComment(blocking bool) fixture {
	return func(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
		token := req.Env["FORGEJO_TOKEN"]
		targetRepo := req.Env["TARGET_REPO"] // "owner/name"
		prNumberStr := req.Env["PR_NUMBER"]
		if token == "" || targetRepo == "" || prNumberStr == "" {
			return 0, nil, fmt.Errorf("fixture %s: env must set FORGEJO_TOKEN, TARGET_REPO, PR_NUMBER", req.PayloadRef)
		}
		prNumber, err := strconv.Atoi(prNumberStr)
		if err != nil {
			return 0, nil, fmt.Errorf("fixture %s: invalid PR_NUMBER %q: %w", req.PayloadRef, prNumberStr, err)
		}
		owner, name, ok := strings.Cut(targetRepo, "/")
		if !ok {
			return 0, nil, fmt.Errorf("fixture %s: TARGET_REPO must be \"owner/name\", got %q", req.PayloadRef, targetRepo)
		}

		forgejoURL := req.Env["FORGEJO_URL"]
		if forgejoURL == "" {
			return 0, nil, fmt.Errorf("fixture %s: env must set FORGEJO_URL", req.PayloadRef)
		}
		git, err := gitclient.NewClient(forgejoURL, token)
		if err != nil {
			return 0, nil, fmt.Errorf("fixture %s: git client: %w", req.PayloadRef, err)
		}

		body := req.Env["COMMENT_BODY"]
		if body == "" {
			body = fmt.Sprintf("launcher-mock fixture %q ran for invocation %s", req.PayloadRef, req.InvocationId)
		}
		if _, err := git.PostPRComment(ctx, gitclient.RepoRef{Owner: owner, Name: name}, prNumber, body); err != nil {
			return 0, nil, fmt.Errorf("fixture %s: post PR comment: %w", req.PayloadRef, err)
		}

		result := `{"has_blocking_finding": false}`
		if blocking {
			result = `{"has_blocking_finding": true}`
		}
		return 0, []byte(result), nil
	}
}
