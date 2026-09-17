//go:build integration

package policyclient_test

import (
	"context"
	"encoding/base64"
	"os"
	"testing"

	giteasdk "code.gitea.io/sdk/gitea"

	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
)

func encodeBase64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestClient_Evaluate(t *testing.T) {
	pdpURL := os.Getenv("PDP_MOCK_URL")
	forgejoURL := os.Getenv("FORGEJO_URL")
	forgejoToken := os.Getenv("FORGEJO_ADMIN_TOKEN")
	owner := os.Getenv("FORGEJO_ADMIN_USERNAME")
	if pdpURL == "" || forgejoURL == "" || forgejoToken == "" || owner == "" {
		t.Skip("PDP_MOCK_URL / FORGEJO_URL / FORGEJO_ADMIN_TOKEN / FORGEJO_ADMIN_USERNAME not set; source deploy/.env first")
	}

	raw, err := giteasdk.NewClient(forgejoURL, giteasdk.SetToken(forgejoToken))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}

	t.Run("resolves steps from a seeded repo", func(t *testing.T) {
		repoName := "policyclient-integration-scratch"
		repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
		if err != nil {
			t.Fatalf("CreateRepo (setup): %v", err)
		}
		t.Cleanup(func() { raw.DeleteRepo(owner, repoName) })

		stagesYAML := `
- id: review-round
  type: agent-stage
  bot_account: review-bot
  task_type: pr-review-round
  blocking: true
  conditions:
    risk_tier: ["low", "medium"]
- id: run-unit-tests
  type: deterministic-step
  bot_account: ci-bot
  task_type: pr-review-round
  blocking: true
  command: "go test ./..."
  working_dir: "."
  timeout_seconds: 120
`
		mustCreateFile(t, raw, owner, repo.Name, ".harness/stages.yaml", stagesYAML)

		client := policyclient.NewClient(pdpURL)
		decision, err := client.Evaluate(context.Background(), policyclient.PolicyRequest{
			TaskID:     "task-1",
			TargetRepo: owner + "/" + repo.Name,
			TaskType:   "pr-review-round",
			RiskTier:   "low",
		})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Decision != "allow" {
			t.Fatalf("expected allow, got %q (reason: %s)", decision.Decision, decision.Reason)
		}
		if len(decision.ResolvedSteps) != 2 {
			t.Fatalf("expected 2 resolved steps, got %+v", decision.ResolvedSteps)
		}
	})

	t.Run("denies fail-closed when .harness/stages.yaml is missing", func(t *testing.T) {
		repoName := "policyclient-integration-nostages"
		repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
		if err != nil {
			t.Fatalf("CreateRepo (setup): %v", err)
		}
		t.Cleanup(func() { raw.DeleteRepo(owner, repoName) })

		client := policyclient.NewClient(pdpURL)
		decision, err := client.Evaluate(context.Background(), policyclient.PolicyRequest{
			TaskID:     "task-2",
			TargetRepo: owner + "/" + repo.Name,
			TaskType:   "pr-review-round",
			RiskTier:   "low",
		})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Decision != "deny" {
			t.Fatalf("expected deny (fail-closed, no stages.yaml), got %q", decision.Decision)
		}
	})

	t.Run("require_human_approval via policy.yaml override resolves an approve-phase step", func(t *testing.T) {
		// Design §4.4: the PDP decision itself stays "allow" — a gate is now
		// expressed as an additional resolved step with phase "approve",
		// the same way "needs a security reviewer" is, not a third Decision
		// value.
		repoName := "policyclient-integration-approval"
		repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
		if err != nil {
			t.Fatalf("CreateRepo (setup): %v", err)
		}
		t.Cleanup(func() { raw.DeleteRepo(owner, repoName) })

		mustCreateFile(t, raw, owner, repo.Name, ".harness/stages.yaml", `
- id: security-triage
  type: agent-stage
  bot_account: review-bot
  task_type: security-triage
  blocking: true
`)
		mustCreateFile(t, raw, owner, repo.Name, ".harness/policy.yaml", `
require_human_approval:
  - step_id: security-triage
`)

		client := policyclient.NewClient(pdpURL)
		decision, err := client.Evaluate(context.Background(), policyclient.PolicyRequest{
			TaskID:     "task-3",
			TargetRepo: owner + "/" + repo.Name,
			TaskType:   "security-triage",
			RiskTier:   "high",
		})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Decision != "allow" {
			t.Fatalf("expected allow (the gate is now an approve-phase resolved step, not a decision value), got %q", decision.Decision)
		}
		foundApprove := false
		for _, s := range decision.ResolvedSteps {
			if s.Phase == policyclient.PhaseApprove {
				foundApprove = true
			}
		}
		if !foundApprove {
			t.Fatalf("expected a resolved step with phase %q, got %+v", policyclient.PhaseApprove, decision.ResolvedSteps)
		}
	})
}

func mustCreateFile(t *testing.T, raw *giteasdk.Client, owner, repo, path, content string) {
	t.Helper()
	_, _, err := raw.CreateFile(owner, repo, path, giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{
			Message:    "add " + path,
			BranchName: "main",
		},
		Content: encodeBase64(content),
	})
	if err != nil {
		t.Fatalf("CreateFile %s: %v", path, err)
	}
}
