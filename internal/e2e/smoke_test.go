//go:build e2e

// Package e2e is Phase 6's one true end-to-end smoke test: full compose
// stack up, `make seed` already run, POST a task against the real seeded
// sample repo, and assert against the real running system — not fakes, not
// a single mock in isolation.
//
// Preconditions this test assumes (documented, not automated here, mirroring
// how test-integration assumes compose-up/bootstrap-admin/migrate already
// ran): `make compose-up && make bootstrap-admin && make migrate && make seed`.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	giteasdk "code.gitea.io/sdk/gitea"

	"github.com/kniemiec/AiCodAgHar/internal/audit"
)

type env struct {
	controlPlaneURL string
	forgejoURL      string
	forgejoToken    string
	owner           string
	auditDSN        string
}

func requireEnv(t *testing.T) env {
	t.Helper()
	e := env{
		controlPlaneURL: os.Getenv("CONTROL_PLANE_URL"),
		forgejoURL:      os.Getenv("FORGEJO_URL"),
		forgejoToken:    os.Getenv("FORGEJO_ADMIN_TOKEN"),
		owner:           os.Getenv("FORGEJO_ADMIN_USERNAME"),
		auditDSN:        os.Getenv("AUDIT_DB_DSN"),
	}
	if e.controlPlaneURL == "" {
		e.controlPlaneURL = "http://localhost:8080"
	}
	if e.forgejoURL == "" || e.forgejoToken == "" || e.owner == "" || e.auditDSN == "" {
		t.Skip("FORGEJO_URL / FORGEJO_ADMIN_TOKEN / FORGEJO_ADMIN_USERNAME / AUDIT_DB_DSN not set; source deploy/.env first, and run `make seed`")
	}
	return e
}

type createTaskRequest struct {
	TargetRepo          string `json:"target_repo"`
	Branch              string `json:"branch"`
	BaseBranch          string `json:"base_branch"`
	TaskType            string `json:"task_type"`
	RiskTier            string `json:"risk_tier"`
	TriggeringPrincipal string `json:"triggering_principal"`
	BotAccount          string `json:"bot_account"`
	PRTitle             string `json:"pr_title"`
	PRBody              string `json:"pr_body"`
}

type stepRunWire struct {
	Round            int             `json:"round"`
	StepID           string          `json:"step_id"`
	StepType         string          `json:"step_type"`
	InvocationID     string          `json:"invocation_id"`
	Blocking         bool            `json:"blocking"`
	ExitCode         int32           `json:"exit_code"`
	HasFinding       bool            `json:"has_finding"`
	StructuredResult json.RawMessage `json:"structured_result,omitempty"`
}

type createTaskResponse struct {
	TaskID    string        `json:"task_id"`
	PRNumber  int           `json:"pr_number"`
	Outcome   string        `json:"outcome"`
	RoundsRun int           `json:"rounds_run"`
	Reason    string        `json:"reason,omitempty"`
	StepRuns  []stepRunWire `json:"step_runs"`
}

func postTask(t *testing.T, controlPlaneURL string, req createTaskRequest) createTaskResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(controlPlaneURL+"/tasks", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /tasks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var msg bytes.Buffer
		msg.ReadFrom(resp.Body)
		t.Fatalf("POST /tasks: unexpected status %d: %s", resp.StatusCode, msg.String())
	}
	var out createTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// TestSmoke_MainFlow drives the five numbered assertions from the plan's
// Testing Strategy against the real seeded sample repo.
func TestSmoke_MainFlow(t *testing.T) {
	e := requireEnv(t)
	raw, err := giteasdk.NewClient(e.forgejoURL, giteasdk.SetToken(e.forgejoToken))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}

	resp := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/sample-repo",
		Branch:              "feature-branch",
		BaseBranch:          "main",
		TaskType:            "pr-review-round",
		RiskTier:            "low",
		TriggeringPrincipal: "e2e-smoke-test",
		BotAccount:          "requesting-account",
		PRTitle:             "e2e smoke test PR",
		PRBody:              "Driven by internal/e2e/smoke_test.go",
	})

	// (5) task status is stop-success.
	if resp.Outcome != "stop-success" {
		t.Fatalf("expected stop-success, got %q (reason: %s)", resp.Outcome, resp.Reason)
	}

	// (1) a new PR exists with a commit trailer containing invocation_id.
	pr, _, err := raw.GetPullRequest(e.owner, "sample-repo", int64(resp.PRNumber))
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.State != "open" {
		t.Fatalf("expected PR #%d to be open, got %q", resp.PRNumber, pr.State)
	}
	commits, _, err := raw.ListRepoCommits(e.owner, "sample-repo", giteasdk.ListCommitOptions{SHA: "feature-branch"})
	if err != nil {
		t.Fatalf("ListRepoCommits: %v", err)
	}
	var reviewInvocationID string
	for _, sr := range resp.StepRuns {
		if sr.StepID == "review-with-commit" {
			reviewInvocationID = sr.InvocationID
		}
	}
	if reviewInvocationID == "" {
		t.Fatal("expected a review-with-commit step_run in the response")
	}
	foundTrailer := false
	for _, c := range commits {
		if c.RepoCommit != nil && strings.Contains(c.RepoCommit.Message, "Invocation-Id: "+reviewInvocationID) {
			foundTrailer = true
			break
		}
	}
	if !foundTrailer {
		t.Fatalf("expected a commit on feature-branch with trailer \"Invocation-Id: %s\"", reviewInvocationID)
	}

	// (2) the PR has >=1 review comment from the agent-stage review executor.
	comments, _, err := raw.ListIssueComments(e.owner, "sample-repo", int64(resp.PRNumber), giteasdk.ListIssueCommentOptions{})
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) < 1 {
		t.Fatal("expected >=1 review comment on the PR")
	}

	// (3) the deterministic-step executor's structured result is present on
	// its step_run.
	var unitTestsResult json.RawMessage
	for _, sr := range resp.StepRuns {
		if sr.StepID == "unit-tests" {
			unitTestsResult = sr.StructuredResult
		}
	}
	if len(unitTestsResult) == 0 {
		t.Fatal("expected a non-empty structured_result on the unit-tests step_run")
	}
	var parsed struct {
		TestsRun int `json:"tests_run"`
	}
	if err := json.Unmarshal(unitTestsResult, &parsed); err != nil || parsed.TestsRun == 0 {
		t.Fatalf("expected a parseable, non-trivial structured result, got: %s", unitTestsResult)
	}

	// (4) invocation_audit has >=3 rows under one task_id with a valid hash
	// chain, queryable well within ADR-5's 5-minute target.
	w, err := audit.NewPostgresWriter(e.auditDSN)
	if err != nil {
		t.Fatalf("NewPostgresWriter: %v", err)
	}
	defer w.Close()
	start := time.Now()
	chain, err := w.GetTaskChain(context.Background(), resp.TaskID)
	if elapsed := time.Since(start); elapsed > 5*time.Minute {
		t.Fatalf("GetTaskChain took %s, exceeding ADR-5's 5-minute target", elapsed)
	}
	if err != nil {
		t.Fatalf("GetTaskChain: %v", err)
	}
	if len(chain) < 3 {
		t.Fatalf("expected >=3 audit rows under task_id %s, got %d", resp.TaskID, len(chain))
	}
	if err := audit.VerifyChain(chain); err != nil {
		t.Fatalf("expected a valid hash chain, got: %v", err)
	}
}

// TestSmoke_RequireHumanApprovalViaBaselinePolicy proves pdp-mock's
// global-baseline-repo merge (plan Decision 7) is live: the sample repo's
// own .harness/policy.yaml does not require human approval for
// review-with-commit at any risk tier, but the seeded global-baseline
// repo's policy.yaml requires it at risk_tier "high" — so a high-risk-tier
// task must escalate on round 1 with zero launches, purely from the
// baseline merge.
func TestSmoke_RequireHumanApprovalViaBaselinePolicy(t *testing.T) {
	e := requireEnv(t)

	resp := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/sample-repo",
		Branch:              "feature-branch",
		BaseBranch:          "main",
		TaskType:            "pr-review-round",
		RiskTier:            "high",
		TriggeringPrincipal: "e2e-smoke-test-baseline",
		BotAccount:          "requesting-account",
	})

	if resp.Outcome != "stop-escalate" {
		t.Fatalf("expected stop-escalate (unapproved require_human_approval from the baseline policy), got %q", resp.Outcome)
	}
	if resp.RoundsRun != 1 {
		t.Fatalf("expected the require_human_approval short-circuit at round 1, got RoundsRun=%d", resp.RoundsRun)
	}
	if !strings.Contains(resp.Reason, "human approval") {
		t.Fatalf("expected reason to mention human approval, got %q", resp.Reason)
	}
	if len(resp.StepRuns) != 0 {
		t.Fatalf("expected zero launches, got %d step_runs", len(resp.StepRuns))
	}
}

// TestSmoke_RoundCapForcesStopEscalate is the plan's "second, narrower
// variant": a repo whose stages.yaml always reports a blocking finding
// forces all 5 rounds — cheap, since launcher-mock's fixtures return in
// milliseconds. Uses its own throwaway repo so it doesn't disturb the main
// flow's seeded sample-repo state.
func TestSmoke_RoundCapForcesStopEscalate(t *testing.T) {
	e := requireEnv(t)
	raw, err := giteasdk.NewClient(e.forgejoURL, giteasdk.SetToken(e.forgejoToken))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}

	repoName := fmt.Sprintf("e2e-escalate-scratch-%d", time.Now().UnixNano())
	repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo (setup): %v", err)
	}
	t.Cleanup(func() { raw.DeleteRepo(e.owner, repoName) })

	stagesYAML := `- id: post-blocking-finding-comment
  type: agent-stage
  bot_account: review-bot
  task_type: pr-review-round
  blocking: true
`
	if _, _, err := raw.CreateFile(e.owner, repo.Name, ".harness/stages.yaml", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add stages.yaml", BranchName: "main"},
		Content:     base64Encode(stagesYAML),
	}); err != nil {
		t.Fatalf("seed stages.yaml: %v", err)
	}
	if _, _, err := raw.CreateFile(e.owner, repo.Name, "feature.txt", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add feature", BranchName: "main", NewBranchName: "feature-branch"},
		Content:     base64Encode("feature"),
	}); err != nil {
		t.Fatalf("seed feature branch: %v", err)
	}

	resp := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/" + repo.Name,
		Branch:              "feature-branch",
		BaseBranch:          "main",
		TaskType:            "pr-review-round",
		RiskTier:            "low",
		TriggeringPrincipal: "e2e-smoke-test-escalate",
		BotAccount:          "requesting-account",
	})

	if resp.Outcome != "stop-escalate" {
		t.Fatalf("expected stop-escalate when a blocking finding persists through the round cap, got %q (reason: %s)", resp.Outcome, resp.Reason)
	}
	if resp.RoundsRun != 5 {
		t.Fatalf("expected all 5 rounds to run, got RoundsRun=%d", resp.RoundsRun)
	}
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
