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
	"slices"
	"strings"
	"testing"
	"time"

	giteasdk "code.gitea.io/sdk/gitea"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
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

// createTaskRequest mirrors cmd/control-plane/handler.go's wire shape
// (Task Intent Propagation design §5): branch is optional — a task no
// longer arrives with one already pushed — and task_type/pr_title/pr_body
// are gone; the server always sets type "new-task" and no longer opens a
// PR from this request at all (coreloop does, once the generate phase
// completes).
type createTaskRequest struct {
	TargetRepo          string `json:"target_repo"`
	Branch              string `json:"branch,omitempty"`
	BaseBranch          string `json:"base_branch"`
	RiskTier            string `json:"risk_tier"`
	TriggeringPrincipal string `json:"triggering_principal"`
	BotAccount          string `json:"bot_account"`
	Description         string `json:"description"`
}

type stepRunWire struct {
	Round            int             `json:"round"`
	StepID           string          `json:"step_id"`
	StepType         string          `json:"step_type"`
	InvocationID     string          `json:"invocation_id"`
	Blocking         bool            `json:"blocking"`
	Status           string          `json:"status"`
	ExitCode         int32           `json:"exit_code"`
	HasFinding       bool            `json:"has_finding"`
	StructuredResult json.RawMessage `json:"structured_result,omitempty"`
}

// createTaskResponse is design §4.3's 202 body — just enough to start
// polling taskStatusResponse.
type createTaskResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// taskStatusResponse is GET /tasks/{id}'s (design §4.3) body. Status uses
// the DB-persisted vocabulary (running/waiting_approval/stop_success/
// stop_escalate), not coreloop.Outcome's hyphenated wire vocabulary.
type taskStatusResponse struct {
	TaskID              string        `json:"task_id"`
	Status              string        `json:"status"`
	CurrentRound        int           `json:"current_round"`
	PRNumber            int           `json:"pr_number"`
	ApprovalIssueNumber *int          `json:"approval_issue_number,omitempty"`
	Reason              string        `json:"reason,omitempty"`
	StepRuns            []stepRunWire `json:"step_runs"`
}

// postTask kicks off a task and returns its ID once control-plane accepts
// it (202) — it does not wait for the task to run at all; pollUntilTerminal
// does that.
func postTask(t *testing.T, controlPlaneURL string, req createTaskRequest) string {
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
	if resp.StatusCode != http.StatusAccepted {
		var msg bytes.Buffer
		msg.ReadFrom(resp.Body)
		t.Fatalf("POST /tasks: unexpected status %d: %s", resp.StatusCode, msg.String())
	}
	var out createTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out.TaskID
}

// pollUntilStatus polls GET /tasks/{id} until status is one of want, or
// fails the test after 2 minutes — generous for a mocked world whose
// launcher fixtures return in milliseconds, but enough headroom for a real
// compose stack under load.
func pollUntilStatus(t *testing.T, controlPlaneURL, taskID string, want ...string) taskStatusResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := http.Get(controlPlaneURL + "/tasks/" + taskID)
		if err != nil {
			t.Fatalf("GET /tasks/%s: %v", taskID, err)
		}
		var status taskStatusResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /tasks/%s: unexpected status %d", taskID, resp.StatusCode)
		}
		if decodeErr != nil {
			t.Fatalf("decode GET /tasks/%s response: %v", taskID, decodeErr)
		}
		if slices.Contains(want, status.Status) {
			return status
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach status in %v within the deadline", taskID, want)
	return taskStatusResponse{}
}

// pollUntilTerminal is pollUntilStatus scoped to the two terminal statuses.
func pollUntilTerminal(t *testing.T, controlPlaneURL, taskID string) taskStatusResponse {
	t.Helper()
	return pollUntilStatus(t, controlPlaneURL, taskID, domain.TaskStatusStopSuccess, domain.TaskStatusStopEscalate)
}

// webhookSecretForRepo reads targetRepo's entry out of
// SECRETS_MOCK_WEBHOOK_POOL_JSON (exported into this process's environment
// by `make test-e2e` sourcing deploy/.env — see deploy/.env.example) —
// design §4.4/§7's per-repo webhook secret can't be handed to a running
// secrets-mock at test time the way everything else here is created fresh
// per run, so it must already be pre-provisioned out of band.
func webhookSecretForRepo(t *testing.T, targetRepo string) string {
	t.Helper()
	raw := os.Getenv("SECRETS_MOCK_WEBHOOK_POOL_JSON")
	if raw == "" {
		t.Skip("SECRETS_MOCK_WEBHOOK_POOL_JSON not set; see deploy/.env.example for how to provision this scenario's scratch-repo secret")
	}
	var pool map[string]string
	if err := json.Unmarshal([]byte(raw), &pool); err != nil {
		t.Fatalf("parse SECRETS_MOCK_WEBHOOK_POOL_JSON: %v", err)
	}
	secret, ok := pool[targetRepo]
	if !ok {
		t.Skipf("SECRETS_MOCK_WEBHOOK_POOL_JSON has no entry for %s; see deploy/.env.example", targetRepo)
	}
	return secret
}

// TestSmoke_MainFlow drives the five numbered assertions from the plan's
// Testing Strategy against the real seeded sample repo.
func TestSmoke_MainFlow(t *testing.T) {
	e := requireEnv(t)
	raw, err := giteasdk.NewClient(e.forgejoURL, giteasdk.SetToken(e.forgejoToken))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}

	taskID := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/sample-repo",
		BaseBranch:          "main",
		RiskTier:            "low",
		TriggeringPrincipal: "e2e-smoke-test",
		BotAccount:          "requesting-account",
		Description:         "Driven by internal/control-plane/e2e/smoke_test.go",
	})
	// Task Intent Propagation design §3/§5: a task no longer arrives with a
	// branch already pushed — the server generates one (task/<task_id>) and
	// the generate-phase step ("implement") creates it fresh off main.
	branch := "task/" + taskID
	resp := pollUntilTerminal(t, e.controlPlaneURL, taskID)

	// (5) task status is stop_success.
	if resp.Status != domain.TaskStatusStopSuccess {
		t.Fatalf("expected stop_success, got %q (reason: %s)", resp.Status, resp.Reason)
	}

	// (1) a new PR exists with a commit trailer containing invocation_id.
	pr, _, err := raw.GetPullRequest(e.owner, "sample-repo", int64(resp.PRNumber))
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.State != "open" {
		t.Fatalf("expected PR #%d to be open, got %q", resp.PRNumber, pr.State)
	}
	commits, _, err := raw.ListRepoCommits(e.owner, "sample-repo", giteasdk.ListCommitOptions{SHA: branch})
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
		t.Fatalf("expected a commit on %s with trailer \"Invocation-Id: %s\"", branch, reviewInvocationID)
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
// task must reach waiting_approval on round 1, purely from the baseline
// merge. Design §4.4 changed what "requires approval" means: it's now an
// approve-phase gate reached only after the round's normal steps (still
// dispatched — an approve gate no longer implies zero launches) find no
// blocking findings, not an immediate pre-launch escalate.
func TestSmoke_RequireHumanApprovalViaBaselinePolicy(t *testing.T) {
	e := requireEnv(t)

	taskID := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/sample-repo",
		BaseBranch:          "main",
		RiskTier:            "high",
		TriggeringPrincipal: "e2e-smoke-test-baseline",
		BotAccount:          "requesting-account",
		Description:         "Driven by internal/control-plane/e2e/smoke_test.go (baseline-policy scenario)",
	})
	resp := pollUntilStatus(t, e.controlPlaneURL, taskID,
		domain.TaskStatusWaitingApproval, domain.TaskStatusStopSuccess, domain.TaskStatusStopEscalate)

	if resp.Status != domain.TaskStatusWaitingApproval {
		t.Fatalf("expected waiting_approval (an approve-phase gate from the baseline policy), got %q (reason: %s)", resp.Status, resp.Reason)
	}
	if resp.ApprovalIssueNumber == nil {
		t.Fatal("expected a non-nil approval_issue_number while waiting_approval")
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

	// Task Intent Propagation design §5: every task now needs a
	// generate-phase step — coreloop only opens the review PR once it
	// completes — so this fixture's always-blocking review step needs an
	// "implement" step ahead of it, the same shape cmd/seed's sample repo
	// uses, or PR_NUMBER would never reach post-blocking-finding-comment's
	// env.
	stagesYAML := `- id: implement
  type: agent-stage
  phase: generate
  bot_account: coding-agent
  task_type: new-task
  blocking: true
- id: post-blocking-finding-comment
  type: agent-stage
  bot_account: review-bot
  task_type: new-task
  blocking: true
`
	if _, _, err := raw.CreateFile(e.owner, repo.Name, ".harness/stages.yaml", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add stages.yaml", BranchName: "main"},
		Content:     base64Encode(stagesYAML),
	}); err != nil {
		t.Fatalf("seed stages.yaml: %v", err)
	}
	// coding-agent needs write access to push the generate-phase step's
	// branch/commit, the same grant cmd/seed's sample repo gives it.
	writeAccess := giteasdk.AccessModeWrite
	if _, err := raw.AddCollaborator(e.owner, repo.Name, "coding-agent", giteasdk.AddCollaboratorOption{Permission: &writeAccess}); err != nil {
		t.Fatalf("add coding-agent as collaborator: %v", err)
	}

	taskID := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/" + repo.Name,
		BaseBranch:          "main",
		RiskTier:            "low",
		TriggeringPrincipal: "e2e-smoke-test-escalate",
		BotAccount:          "requesting-account",
		Description:         "Driven by internal/control-plane/e2e/smoke_test.go (round-cap scenario)",
	})
	resp := pollUntilTerminal(t, e.controlPlaneURL, taskID)

	if resp.Status != domain.TaskStatusStopEscalate {
		t.Fatalf("expected stop_escalate when a blocking finding persists through the round cap, got %q (reason: %s)", resp.Status, resp.Reason)
	}
	if resp.CurrentRound != 5 {
		t.Fatalf("expected all 5 rounds to run, got current_round=%d", resp.CurrentRound)
	}
}

// TestSmoke_ApprovalWebhookFlow is design §9's E2E scenario for §4.4: POST a
// task whose policy.yaml requires human approval, poll until
// waiting_approval, label the real approval issue "approved" on the real
// Forgejo instance (not a simulated webhook call), and assert that the
// resulting real webhook delivery resumes the task to stop-success.
//
// Uses a fixed (not timestamped) scratch repo, unlike the other scenarios'
// throwaway repos: its webhook secret has to be pre-provisioned into
// secrets-mock out of band (SECRETS_MOCK_WEBHOOK_POOL_JSON), which only
// works for a name known ahead of time.
func TestSmoke_ApprovalWebhookFlow(t *testing.T) {
	e := requireEnv(t)
	repoName := "e2e-approval-scratch"
	secret := webhookSecretForRepo(t, e.owner+"/"+repoName)

	raw, err := giteasdk.NewClient(e.forgejoURL, giteasdk.SetToken(e.forgejoToken))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}
	raw.DeleteRepo(e.owner, repoName) // best-effort: a prior interrupted run may have left this behind
	repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo (setup): %v", err)
	}
	t.Cleanup(func() { raw.DeleteRepo(e.owner, repoName) })

	// Task Intent Propagation design §5: every task now needs a
	// generate-phase step ahead of any review-phase step needing PR_NUMBER
	// (post-pr-comment here) — coreloop only opens the PR once it completes.
	stagesYAML := `- id: implement
  type: agent-stage
  phase: generate
  bot_account: coding-agent
  task_type: new-task
  blocking: true
- id: post-pr-comment
  type: agent-stage
  bot_account: review-bot
  task_type: new-task
  blocking: false
`
	if _, _, err := raw.CreateFile(e.owner, repo.Name, ".harness/stages.yaml", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add stages.yaml", BranchName: "main"},
		Content:     base64Encode(stagesYAML),
	}); err != nil {
		t.Fatalf("seed stages.yaml: %v", err)
	}
	if _, _, err := raw.CreateFile(e.owner, repo.Name, ".harness/policy.yaml", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add policy.yaml", BranchName: "main"},
		Content:     base64Encode("require_human_approval:\n  - step_id: post-pr-comment\n"),
	}); err != nil {
		t.Fatalf("seed policy.yaml: %v", err)
	}
	if _, _, err := raw.CreateFile(e.owner, repo.Name, ".harness/APPROVERS", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add APPROVERS", BranchName: "main"},
		Content:     base64Encode(e.owner + "\n"),
	}); err != nil {
		t.Fatalf("seed APPROVERS: %v", err)
	}
	// coding-agent needs write access to push the generate-phase step's
	// branch/commit, the same grant cmd/seed's sample repo gives it. No
	// feature branch is pre-seeded anymore — the generate-phase step
	// creates the task's branch itself, fresh off main.
	writeAccess := giteasdk.AccessModeWrite
	if _, err := raw.AddCollaborator(e.owner, repo.Name, "coding-agent", giteasdk.AddCollaboratorOption{Permission: &writeAccess}); err != nil {
		t.Fatalf("add coding-agent as collaborator: %v", err)
	}

	if _, _, err := raw.CreateRepoHook(e.owner, repo.Name, giteasdk.CreateHookOption{
		Type:   giteasdk.HookTypeGitea,
		Config: map[string]string{"url": "http://control-plane:8080/webhooks/forgejo", "content_type": "json", "secret": secret},
		Events: []string{"issues"},
		Active: true,
	}); err != nil {
		t.Fatalf("CreateHook: %v", err)
	}

	approvedLabel, _, err := raw.CreateLabel(e.owner, repo.Name, giteasdk.CreateLabelOption{Name: "approved", Color: "00ff00"})
	if err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}

	taskID := postTask(t, e.controlPlaneURL, createTaskRequest{
		TargetRepo:          e.owner + "/" + repoName,
		BaseBranch:          "main",
		RiskTier:            "low",
		TriggeringPrincipal: "e2e-smoke-test-approval",
		BotAccount:          "requesting-account",
		Description:         "Driven by internal/control-plane/e2e/smoke_test.go (approval-webhook scenario)",
	})

	waiting := pollUntilStatus(t, e.controlPlaneURL, taskID, domain.TaskStatusWaitingApproval)
	if waiting.ApprovalIssueNumber == nil {
		t.Fatal("expected a non-nil approval_issue_number while waiting_approval")
	}

	if _, _, err := raw.AddIssueLabels(e.owner, repo.Name, int64(*waiting.ApprovalIssueNumber), giteasdk.IssueLabelsOption{
		Labels: []int64{approvedLabel.ID},
	}); err != nil {
		t.Fatalf("AddIssueLabels: %v", err)
	}

	final := pollUntilTerminal(t, e.controlPlaneURL, taskID)
	if final.Status != domain.TaskStatusStopSuccess {
		t.Fatalf("expected stop_success after a real approval webhook, got %q (reason: %s)", final.Status, final.Reason)
	}
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
