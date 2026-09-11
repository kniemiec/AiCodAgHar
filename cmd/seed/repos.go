package main

import (
	"context"
	"fmt"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

const (
	// GlobalBaselineRepoName is the platform-owned global-baseline policy
	// repo (plan Decision 3) — separate from any target repo, matching
	// ADR-9/ADR-6's real shape.
	GlobalBaselineRepoName = "platform-baseline-policy"
	// SampleTargetRepoName is the sample repo the orchestrator actually
	// operates against (plan Decision 3).
	SampleTargetRepoName = "sample-repo"

	sampleFeatureBranch = "feature-branch"
)

// globalBaselinePolicyYAML requires human approval for review-with-commit
// at risk_tier "high" — scoped so it never fires during the low-risk-tier
// main e2e flow, but is independently verifiable: the sample repo's own
// .harness/policy.yaml is intentionally NOT seeded, so a require_human_
// approval decision at risk_tier=high can only be coming from this
// baseline repo, proving pdp-mock's baseline-merge path (plan Decision 7)
// is really live, not just present in code.
const globalBaselinePolicyYAML = `require_human_approval:
  - step_id: review-with-commit
    risk_tier: ["high"]
`

// sampleStagesYAML: ≥1 agent-stage entry and ≥1 deterministic-step entry,
// as Phase 6 requires. Step IDs double as launcher-mock fixture keys in
// this mocked environment (see internal/executor's PayloadRef doc
// comments) — "review-with-commit" and "unit-tests" are real fixtures in
// cmd/launcher-mock/fixtures.go.
const sampleStagesYAML = `- id: review-with-commit
  type: agent-stage
  bot_account: review-bot
  task_type: pr-review-round
  blocking: true
- id: unit-tests
  type: deterministic-step
  bot_account: ci-bot
  task_type: pr-review-round
  blocking: true
  command: "go test ./..."
  working_dir: "."
  timeout_seconds: 120
`

const sampleFeatureContent = "feature placeholder content, seeded by cmd/seed\n"

// SeedRepos recreates the global-baseline and sample target repos from
// scratch (delete-then-recreate is this CLI's idempotency strategy — see
// package doc) and returns the sample repo's ref for the caller to run
// tasks against.
func SeedRepos(ctx context.Context, git *gitclient.Client, owner string) (gitclient.RepoRef, error) {
	baselineRepo := gitclient.RepoRef{Owner: owner, Name: GlobalBaselineRepoName}
	_ = git.DeleteRepo(ctx, baselineRepo) // best-effort; "did not exist" is fine
	if _, err := git.CreateRepo(ctx, GlobalBaselineRepoName, true); err != nil {
		return gitclient.RepoRef{}, fmt.Errorf("create global-baseline repo: %w", err)
	}
	if err := git.CreateFile(ctx, baselineRepo, ".harness/policy.yaml", "main", "seed: add global-baseline policy", []byte(globalBaselinePolicyYAML)); err != nil {
		return gitclient.RepoRef{}, fmt.Errorf("seed global-baseline policy.yaml: %w", err)
	}

	targetRepo := gitclient.RepoRef{Owner: owner, Name: SampleTargetRepoName}
	_ = git.DeleteRepo(ctx, targetRepo)
	if _, err := git.CreateRepo(ctx, SampleTargetRepoName, true); err != nil {
		return gitclient.RepoRef{}, fmt.Errorf("create sample target repo: %w", err)
	}
	// AutoInit's default README.md already satisfies "trivial source
	// content" — no need to add another one.
	if err := git.CreateFile(ctx, targetRepo, ".harness/stages.yaml", "main", "seed: add stages.yaml", []byte(sampleStagesYAML)); err != nil {
		return gitclient.RepoRef{}, fmt.Errorf("seed stages.yaml: %w", err)
	}
	// .harness/policy.yaml is deliberately not seeded — see
	// globalBaselinePolicyYAML's doc comment.
	if err := git.CreateFileOnNewBranch(ctx, targetRepo, "feature.txt", "main", sampleFeatureBranch, "seed: add feature branch", []byte(sampleFeatureContent)); err != nil {
		return gitclient.RepoRef{}, fmt.Errorf("seed feature branch: %w", err)
	}

	// The bot accounts stages.yaml assigns steps to need write access to
	// actually commit/comment when launcher-mock's fixtures run as them.
	for _, bot := range botAccounts {
		if err := git.AddCollaborator(ctx, targetRepo, bot.Username); err != nil {
			return gitclient.RepoRef{}, fmt.Errorf("add %s as collaborator: %w", bot.Username, err)
		}
	}

	return targetRepo, nil
}
