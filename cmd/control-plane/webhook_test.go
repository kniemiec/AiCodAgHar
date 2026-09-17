package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/approval"
	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
	"github.com/kniemiec/AiCodAgHar/internal/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/taskrunner"
)

const webhookTestSecret = "test-webhook-secret"
const webhookTestRepo = "acme/widgets"

// newWebhookTestServer wires a server whose only task ("task-1") is already
// parked waiting_approval on issue #12 — the state every test below starts
// from, exercising just the webhook handler's own verification chain.
func newWebhookTestServer(t *testing.T) (*server, *fakes.FakeGitHostClient, *fakes.FakeTaskStore) {
	t.Helper()
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "human-signoff", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhaseApprove},
					{StepID: "notify", Type: executor.StepTypeDeterministicStep, Phase: policyclient.PhasePublish},
				},
			}, nil
		},
	}
	secrets := &fakes.FakeSecretsClient{}
	auditWriter := &fakes.FakeAuditWriter{}
	git := &fakes.FakeGitHostClient{}
	store := &fakes.FakeTaskStore{}
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
	)
	runner := taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy:      policy,
			Approval:    &approval.RealApprovalVerifier{Reader: taskrunner.NewTaskApprovalReader(store)},
			Audit:       auditWriter,
			Executors:   registry,
			IssueOpener: git,
		},
		Store:    store,
		Launcher: launcher,
	})

	ctx := context.Background()
	if _, done, err := runner.Start(ctx, domain.Task{ID: "task-1", TargetRepo: webhookTestRepo, BaseBranch: "main"}); err != nil {
		t.Fatalf("Start: %v", err)
	} else if result := <-done; result.Outcome != coreloop.OutcomeWaitingApproval {
		t.Fatalf("expected the fixture task to start out waiting-approval, got %v", result.Outcome)
	}

	git.SetCollaboratorRole(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, "alice", gitclient.RoleWrite)
	git.SetFileContents(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, "main", approversPath, []byte("alice\n"))

	secrets.WebhookSecrets = map[string]string{webhookTestRepo: webhookTestSecret}

	return &server{git: git, secrets: secrets, runner: runner}, git, store
}

func postSignedWebhook(t *testing.T, mux http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(webhookTestSecret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Signature", sig)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func labelWebhookBody(t *testing.T, issueNumber int, senderLogin string) []byte {
	t.Helper()
	body, err := json.Marshal(forgejoIssueWebhook{
		Action: "label_updated",
		Issue: struct {
			Number int `json:"number"`
		}{Number: issueNumber},
		Repository: struct {
			FullName string `json:"full_name"`
		}{FullName: webhookTestRepo},
		Sender: struct {
			Login string `json:"login"`
		}{Login: senderLogin},
	})
	if err != nil {
		t.Fatalf("marshal webhook body: %v", err)
	}
	return body
}

func TestHandleForgejoWebhook_BadSignatureIs403(t *testing.T) {
	s, git, _ := newWebhookTestServer(t)
	git.SetIssueLabels(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, 1, []string{"approved"})
	mux := newServeMux(s)

	body := labelWebhookBody(t, 1, "alice")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Signature", "not-a-real-signature")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on a bad signature, got %d", rec.Code)
	}
}

func TestHandleForgejoWebhook_ApprovedByListedWriteCollaboratorResumesToStopSuccess(t *testing.T) {
	s, git, store := newWebhookTestServer(t)
	mux := newServeMux(s)

	rec, ok, err := store.GetTask(context.Background(), "task-1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	issueNumber := *rec.State.ApprovalIssueNumber
	git.SetIssueLabels(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, issueNumber, []string{"approved"})

	resp := postSignedWebhook(t, mux, labelWebhookBody(t, issueNumber, "alice"))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	waitForStatus(t, store, "task-1", domain.TaskStatusStopSuccess)
}

func TestHandleForgejoWebhook_RejectedEscalates(t *testing.T) {
	s, git, store := newWebhookTestServer(t)
	mux := newServeMux(s)

	rec, _, _ := store.GetTask(context.Background(), "task-1")
	issueNumber := *rec.State.ApprovalIssueNumber
	git.SetIssueLabels(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, issueNumber, []string{"rejected"})

	resp := postSignedWebhook(t, mux, labelWebhookBody(t, issueNumber, "alice"))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	waitForStatus(t, store, "task-1", domain.TaskStatusStopEscalate)
}

func TestHandleForgejoWebhook_LabelerNotInApproversStaysWaiting(t *testing.T) {
	s, git, store := newWebhookTestServer(t)
	mux := newServeMux(s)

	rec, _, _ := store.GetTask(context.Background(), "task-1")
	issueNumber := *rec.State.ApprovalIssueNumber
	git.SetIssueLabels(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, issueNumber, []string{"approved"})
	// "mallory" is not listed in the fixture's APPROVERS file, even though
	// she might genuinely hold Write access to the repo.
	git.SetCollaboratorRole(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, "mallory", gitclient.RoleWrite)

	resp := postSignedWebhook(t, mux, labelWebhookBody(t, issueNumber, "mallory"))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 (fail closed, not an error response), got %d: %s", resp.Code, resp.Body.String())
	}

	rec, _, _ = store.GetTask(context.Background(), "task-1")
	if rec.State.Status != domain.TaskStatusWaitingApproval {
		t.Fatalf("expected the task to stay waiting_approval, got %q", rec.State.Status)
	}
}

func TestHandleForgejoWebhook_ApproverWithoutLiveWriteAccessStaysWaiting(t *testing.T) {
	s, git, store := newWebhookTestServer(t)
	mux := newServeMux(s)

	rec, _, _ := store.GetTask(context.Background(), "task-1")
	issueNumber := *rec.State.ApprovalIssueNumber
	git.SetIssueLabels(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, issueNumber, []string{"approved"})
	// Listed in APPROVERS but no longer a live collaborator on the repo at
	// all — a stale APPROVERS entry. (No SetCollaboratorRole call:
	// VerifyCollaboratorRole defaults to false.)
	git.SetFileContents(gitclient.RepoRef{Owner: "acme", Name: "widgets"}, "main", approversPath, []byte("alice\nalice-departed\n"))

	resp := postSignedWebhook(t, mux, labelWebhookBody(t, issueNumber, "alice-departed"))
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	rec, _, _ = store.GetTask(context.Background(), "task-1")
	if rec.State.Status != domain.TaskStatusWaitingApproval {
		t.Fatalf("expected the task to stay waiting_approval, got %q", rec.State.Status)
	}
}

func waitForStatus(t *testing.T, store *fakes.FakeTaskStore, taskID, want string) {
	t.Helper()
	// The webhook handler dispatches Resume in its own goroutine (design
	// §4.4 step 5, so the webhook response itself returns fast); poll
	// briefly rather than racing it. The fakes do no real I/O, so this
	// settles within a handful of iterations in practice.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok, err := store.GetTask(context.Background(), taskID)
		if err == nil && ok && rec.State.Status == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	rec, _, _ := store.GetTask(context.Background(), taskID)
	t.Fatalf("expected task %s to reach status %q, got %q", taskID, want, rec.State.Status)
}
