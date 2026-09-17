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
	"implement":                     fixtureImplement,
	"review-with-commit":            fixtureReviewWithCommit,
	"unit-tests":                    fixtureUnitTests,
}

func fixtureNoopSuccess(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	return 0, []byte(`{"has_blocking_finding": false}`), nil
}

func fixtureDeterministicFail(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	return 1, nil, nil
}

// gitEnv is the credential + repo/PR/branch context every real-side-effect
// fixture needs from LaunchRequest.env.
type gitEnv struct {
	client   *gitclient.Client
	repo     gitclient.RepoRef
	prNumber int
	branch   string
}

func parseGitEnv(req *launcherv1.LaunchRequest) (gitEnv, error) {
	token := req.Env["FORGEJO_TOKEN"]
	targetRepo := req.Env["TARGET_REPO"] // "owner/name"
	forgejoURL := req.Env["FORGEJO_URL"]
	if token == "" || targetRepo == "" || forgejoURL == "" {
		return gitEnv{}, fmt.Errorf("fixture %s: env must set FORGEJO_TOKEN, TARGET_REPO, FORGEJO_URL", req.PayloadRef)
	}
	owner, name, ok := strings.Cut(targetRepo, "/")
	if !ok {
		return gitEnv{}, fmt.Errorf("fixture %s: TARGET_REPO must be \"owner/name\", got %q", req.PayloadRef, targetRepo)
	}
	client, err := gitclient.NewClient(forgejoURL, token)
	if err != nil {
		return gitEnv{}, fmt.Errorf("fixture %s: git client: %w", req.PayloadRef, err)
	}

	prNumber := 0
	if s := req.Env["PR_NUMBER"]; s != "" {
		prNumber, err = strconv.Atoi(s)
		if err != nil {
			return gitEnv{}, fmt.Errorf("fixture %s: invalid PR_NUMBER %q: %w", req.PayloadRef, s, err)
		}
	}

	return gitEnv{
		client:   client,
		repo:     gitclient.RepoRef{Owner: owner, Name: name},
		prNumber: prNumber,
		branch:   req.Env["BRANCH"],
	}, nil
}

// fixtureImplement simulates the generate phase's coding agent (design
// docs/designs/completed/full-pipeline-async-approval-design.md §4.1): on round
// 1 — no PRIOR_FINDINGS_JSON in env — it commits an initial implementation;
// on later rounds it commits a fix addressing the previous round's
// findings, which it treats as untrusted review output, not instructions
// (§7), only ever quoting it verbatim into its own commit's body.
func fixtureImplement(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	env, err := parseGitEnv(req)
	if err != nil {
		return 0, nil, err
	}
	if env.branch == "" {
		return 0, nil, fmt.Errorf("fixture %s: env must set BRANCH", req.PayloadRef)
	}

	priorFindings := req.Env["PRIOR_FINDINGS_JSON"]
	var commitMsg string
	var content []byte
	if priorFindings == "" {
		commitMsg = fmt.Sprintf("Add initial implementation\n\nInvocation-Id: %s", req.InvocationId)
		content = fmt.Appendf(nil, "Initial implementation placeholder, committed by launcher-mock fixture %q for invocation %s.\n", req.PayloadRef, req.InvocationId)
	} else {
		commitMsg = fmt.Sprintf("Address prior review findings\n\nInvocation-Id: %s", req.InvocationId)
		content = fmt.Appendf(nil, "Fix addressing prior findings, committed by launcher-mock fixture %q for invocation %s.\n\nPrior findings (untrusted review output, quoted verbatim):\n%s\n", req.PayloadRef, req.InvocationId, priorFindings)
	}

	notesPath := fmt.Sprintf(".implement-notes/%s.md", strings.NewReplacer(":", "-", "/", "-").Replace(req.InvocationId))
	if err := env.client.CreateFile(ctx, env.repo, notesPath, env.branch, commitMsg, content); err != nil {
		return 0, nil, fmt.Errorf("fixture %s: commit implementation: %w", req.PayloadRef, err)
	}

	return 0, []byte(`{"has_blocking_finding": false}`), nil
}

// fixtureReviewWithCommit simulates a review stage that both leaves a real
// commit — with an Invocation-Id trailer, so the e2e smoke test can find
// its way from a PR's HEAD commit back to the invocation that authored it
// (ADR-2's delegation record, made git-visible) — and posts a review
// comment, using the credential injected for this invocation.
func fixtureReviewWithCommit(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	env, err := parseGitEnv(req)
	if err != nil {
		return 0, nil, err
	}
	if env.branch == "" || env.prNumber == 0 {
		return 0, nil, fmt.Errorf("fixture %s: env must set BRANCH and PR_NUMBER", req.PayloadRef)
	}

	// Path is unique per invocation (not a fixed "REVIEW_NOTES.md") so
	// re-running this fixture against the same branch — e.g. re-running the
	// e2e smoke test without re-seeding — never collides with a prior run's
	// commit; each invocation leaves its own trailer-bearing commit.
	commitMsg := fmt.Sprintf("Add automated review notes\n\nInvocation-Id: %s", req.InvocationId)
	notes := fmt.Appendf(nil, "Reviewed by launcher-mock fixture %q for invocation %s.\n", req.PayloadRef, req.InvocationId)
	notesPath := fmt.Sprintf(".review-notes/%s.md", strings.NewReplacer(":", "-", "/", "-").Replace(req.InvocationId))
	if err := env.client.CreateFile(ctx, env.repo, notesPath, env.branch, commitMsg, notes); err != nil {
		return 0, nil, fmt.Errorf("fixture %s: commit review notes: %w", req.PayloadRef, err)
	}

	commentBody := fmt.Sprintf("Automated review complete for invocation %s. No blocking issues found.", req.InvocationId)
	if _, err := env.client.PostPRComment(ctx, env.repo, env.prNumber, commentBody); err != nil {
		return 0, nil, fmt.Errorf("fixture %s: post PR comment: %w", req.PayloadRef, err)
	}

	return 0, []byte(`{"has_blocking_finding": false}`), nil
}

// fixtureUnitTests simulates a deterministic-step's structured test-summary
// output — the e2e smoke test asserts this lands on the step_run.
func fixtureUnitTests(ctx context.Context, req *launcherv1.LaunchRequest) (int32, []byte, error) {
	return 0, []byte(`{"tests_run":12,"tests_passed":12,"coverage_percent":87.5}`), nil
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
