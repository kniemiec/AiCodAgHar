package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/config"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// maxWebhookBodyBytes bounds how much of a webhook request body this
// handler will read — a public endpoint, so a defensive cap against an
// oversized payload before it's ever parsed.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// approversPath is where design §4.4 says a repo's authorization list for
// approving/rejecting a task lives — co-located with .harness/stages.yaml
// and .harness/policy.yaml rather than a repo-root convention.
const approversPath = ".harness/APPROVERS"

const (
	labelApproved = "approved"
	labelRejected = "rejected"
)

// forgejoIssueWebhook is the subset of a Forgejo "issues" webhook payload
// (fired on label_updated and opened, among other issue events) this
// handler needs. repository.full_name is "owner/name", matching
// domain.Task.TargetRepo's own convention directly. Title/Body are only
// populated on an "opened" delivery — Task Intent Propagation design §3's
// task-creation trigger, where the issue body becomes the task description.
type forgejoIssueWebhook struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// handleForgejoWebhook implements design §4.4 step B: verify the delivery's
// HMAC signature, then independently re-verify (never trusting the payload
// itself) that a label change on a known waiting_approval task's issue is a
// genuine approval/rejection from someone in that repo's APPROVERS file who
// still holds at-least-Write collaborator access. Any failure along the way
// is fail-closed: the task simply stays waiting_approval, logged but not
// escalated, since a misconfigured or momentarily-unreachable check must
// never manufacture a false approval.
func (s *server) handleForgejoWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var payload forgejoIssueWebhook
	if err := json.Unmarshal(body, &payload); err != nil || payload.Repository.FullName == "" {
		http.Error(w, "invalid or incomplete webhook payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Verifying the signature requires knowing which repo's secret to check
	// against, which itself requires this much of the payload — that's
	// inherent to a per-repo-secret HMAC scheme. Nothing beyond
	// repository.full_name is trusted until the signature check below passes.
	secret, err := s.secrets.GetWebhookSecret(ctx, payload.Repository.FullName)
	if err != nil {
		slog.WarnContext(ctx, "webhook: no secret provisioned", "repo", payload.Repository.FullName, "error", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if !verifyForgejoSignature(secret, body, r.Header.Get("X-Forgejo-Signature")) {
		slog.WarnContext(ctx, "webhook: signature verification failed", "repo", payload.Repository.FullName)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	owner, name, ok := strings.Cut(payload.Repository.FullName, "/")
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	repo := gitclient.RepoRef{Owner: owner, Name: name}

	switch payload.Action {
	case "opened":
		s.handleIssueOpened(ctx, w, repo, payload)
		return
	case "label_updated":
		// falls through to the approval/rejection handling below
	default:
		w.WriteHeader(http.StatusOK)
		return
	}

	taskID, ok, err := s.runner.FindWaitingApprovalTask(ctx, payload.Repository.FullName, payload.Issue.Number)
	if err != nil {
		slog.ErrorContext(ctx, "webhook: find waiting-approval task failed", "repo", payload.Repository.FullName, "issue", payload.Issue.Number, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx = logging.WithTask(ctx, taskID)

	labels, err := s.git.ListIssueLabels(ctx, repo, payload.Issue.Number)
	if err != nil {
		slog.ErrorContext(ctx, "webhook: list labels failed", "repo", payload.Repository.FullName, "issue", payload.Issue.Number, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	rejected := slices.Contains(labels, labelRejected)
	approved := slices.Contains(labels, labelApproved)
	if !rejected && !approved {
		w.WriteHeader(http.StatusOK)
		return
	}

	status, ok, err := s.runner.GetTaskStatus(ctx, taskID)
	if err != nil || !ok {
		slog.ErrorContext(ctx, "webhook: load task failed", "found", ok, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	approversRaw, err := s.git.GetFileContents(ctx, repo, approversPath, status.Task.BaseBranch)
	if err != nil {
		slog.ErrorContext(ctx, "webhook: read approvers file failed", "path", approversPath, "repo", payload.Repository.FullName, "ref", status.Task.BaseBranch, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !slices.Contains(config.ParseApprovers(approversRaw), payload.Sender.Login) {
		slog.WarnContext(ctx, "webhook: sender is not a listed approver", "sender", payload.Sender.Login, "path", approversPath, "repo", payload.Repository.FullName)
		w.WriteHeader(http.StatusOK)
		return
	}
	hasWrite, err := s.git.VerifyCollaboratorRole(ctx, repo, payload.Sender.Login, gitclient.RoleWrite)
	if err != nil || !hasWrite {
		slog.WarnContext(ctx, "webhook: sender failed live collaborator-role check", "sender", payload.Sender.Login, "repo", payload.Repository.FullName, "has_write", hasWrite, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	label := labelApproved
	if rejected {
		label = labelRejected
	}
	evidence := "issue " + payload.Repository.FullName + "#" + strconv.Itoa(payload.Issue.Number) + " labeled \"" + label + "\" by " + payload.Sender.Login

	if rejected {
		if err := s.runner.RecordRejection(ctx, taskID, payload.Sender.Login, evidence); err != nil {
			slog.ErrorContext(ctx, "webhook: record rejection failed", "error", err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := s.runner.RecordApproval(ctx, taskID, payload.Sender.Login, evidence); err != nil {
		slog.ErrorContext(ctx, "webhook: record approval failed", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	// Resume in a new goroutine (design §4.4 step 5) so the webhook
	// response returns fast — the task's execution must outlive this
	// request the same way Start's does.
	resumeCtx := logging.WithTask(context.Background(), taskID)
	go func() {
		result, err := s.runner.Resume(resumeCtx, taskID)
		if err != nil {
			slog.ErrorContext(resumeCtx, "webhook: resume failed", "error", err)
			return
		}
		if result.Err != nil {
			slog.ErrorContext(resumeCtx, "webhook: resume ended with error", "error", result.Err)
		}
	}()
	w.WriteHeader(http.StatusOK)
}

// handleIssueOpened implements Task Intent Propagation design §3: a Forgejo
// issue-opened event, once its signature has already verified (the caller's
// job), starts a new task whose description is the issue body. taskID is
// deterministic from (repo, issue number) so a webhook redelivery is a
// no-op rather than a duplicate task. Authorization only requires "not an
// arbitrary public stranger" — live Write collaborator access, the same
// check the approval path already performs — since task creation, unlike
// approving a publish, isn't itself irreversible. Every failure path is
// fail-closed: logged, no task created, 200 returned regardless (never
// revealing to an unauthorized caller why nothing happened).
func (s *server) handleIssueOpened(ctx context.Context, w http.ResponseWriter, repo gitclient.RepoRef, payload forgejoIssueWebhook) {
	taskID := fmt.Sprintf("issue-%s-%d", strings.ReplaceAll(payload.Repository.FullName, "/", "-"), payload.Issue.Number)
	ctx = logging.WithTask(ctx, taskID)

	if _, ok, err := s.runner.GetTaskStatus(ctx, taskID); err != nil {
		slog.ErrorContext(ctx, "webhook: check existing task failed", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	} else if ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	hasWrite, err := s.git.VerifyCollaboratorRole(ctx, repo, payload.Sender.Login, gitclient.RoleWrite)
	if err != nil || !hasWrite {
		slog.WarnContext(ctx, "webhook: sender failed live collaborator-role check", "sender", payload.Sender.Login, "repo", payload.Repository.FullName, "has_write", hasWrite, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	task := domain.Task{
		ID:                  taskID,
		Type:                "new-task",
		TargetRepo:          payload.Repository.FullName,
		TriggeringPrincipal: payload.Sender.Login,
		RiskTier:            "low",
		Branch:              fmt.Sprintf("task/issue-%d", payload.Issue.Number),
		BaseBranch:          "main",
		Description:         payload.Issue.Body,
	}
	if _, _, err := s.runner.Start(ctx, task); err != nil {
		slog.ErrorContext(ctx, "webhook: start task failed", "error", err)
	}
	w.WriteHeader(http.StatusOK)
}

// verifyForgejoSignature checks body's HMAC-SHA256 (hex-encoded) against
// Forgejo's X-Forgejo-Signature header, using a constant-time comparison.
func verifyForgejoSignature(secret string, body []byte, signatureHeader string) bool {
	if signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}
