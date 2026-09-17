package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/config"
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
// (fired on label_updated, among other issue events) this handler needs.
// repository.full_name is "owner/name", matching domain.Task.TargetRepo's
// own convention directly.
type forgejoIssueWebhook struct {
	Action string `json:"action"`
	Issue  struct {
		Number int `json:"number"`
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
		log.Printf("webhook: no secret provisioned for %s: %v", payload.Repository.FullName, err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if !verifyForgejoSignature(secret, body, r.Header.Get("X-Forgejo-Signature")) {
		log.Printf("webhook: signature verification failed for %s", payload.Repository.FullName)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if payload.Action != "label_updated" {
		w.WriteHeader(http.StatusOK)
		return
	}

	owner, name, ok := strings.Cut(payload.Repository.FullName, "/")
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	repo := gitclient.RepoRef{Owner: owner, Name: name}

	taskID, ok, err := s.runner.FindWaitingApprovalTask(ctx, payload.Repository.FullName, payload.Issue.Number)
	if err != nil {
		log.Printf("webhook: find waiting-approval task for %s#%d: %v", payload.Repository.FullName, payload.Issue.Number, err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	labels, err := s.git.ListIssueLabels(ctx, repo, payload.Issue.Number)
	if err != nil {
		log.Printf("webhook: list labels for %s#%d: %v", payload.Repository.FullName, payload.Issue.Number, err)
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
		log.Printf("webhook: load task %s: ok=%v err=%v", taskID, ok, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	approversRaw, err := s.git.GetFileContents(ctx, repo, approversPath, status.Task.BaseBranch)
	if err != nil {
		log.Printf("webhook: read %s on %s@%s: %v", approversPath, payload.Repository.FullName, status.Task.BaseBranch, err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !slices.Contains(config.ParseApprovers(approversRaw), payload.Sender.Login) {
		log.Printf("webhook: %s is not listed in %s on %s", payload.Sender.Login, approversPath, payload.Repository.FullName)
		w.WriteHeader(http.StatusOK)
		return
	}
	hasWrite, err := s.git.VerifyCollaboratorRole(ctx, repo, payload.Sender.Login, gitclient.RoleWrite)
	if err != nil || !hasWrite {
		log.Printf("webhook: %s failed live collaborator-role check on %s: hasWrite=%v err=%v", payload.Sender.Login, payload.Repository.FullName, hasWrite, err)
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
			log.Printf("webhook: record rejection for %s: %v", taskID, err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := s.runner.RecordApproval(ctx, taskID, payload.Sender.Login, evidence); err != nil {
		log.Printf("webhook: record approval for %s: %v", taskID, err)
		w.WriteHeader(http.StatusOK)
		return
	}
	// Resume in a new goroutine (design §4.4 step 5) so the webhook
	// response returns fast — the task's execution must outlive this
	// request the same way Start's does.
	go func() {
		result, err := s.runner.Resume(context.Background(), taskID)
		if err != nil {
			log.Printf("webhook: resume %s: %v", taskID, err)
			return
		}
		if result.Err != nil {
			log.Printf("webhook: resume %s: %v", taskID, result.Err)
		}
	}()
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
