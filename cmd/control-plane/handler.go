package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
)

// createTaskRequest's branch is now optional (Task Intent Propagation
// design §5): a task no longer arrives with a branch/PR already open, so
// when omitted the server generates one the same way the issue-opened
// webhook trigger does. task_type is accepted but ignored — the server
// always sets domain.Task.Type to "new-task", the only live value now that
// pr-review-round is retired. pr_title/pr_body are gone too: PR creation no
// longer happens here at all (§5), so there's nothing left for them to
// configure.
type createTaskRequest struct {
	TaskID              string `json:"task_id"`
	TargetRepo          string `json:"target_repo"`
	Branch              string `json:"branch"`
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

func stepRunsToWire(runs []domain.StepRun) []stepRunWire {
	out := make([]stepRunWire, len(runs))
	for i, sr := range runs {
		out[i] = stepRunWire{
			Round: sr.Round, StepID: sr.StepID, StepType: sr.StepType, InvocationID: sr.InvocationID,
			Blocking: sr.Blocking, Status: sr.Status, ExitCode: sr.ExitCode, HasFinding: sr.HasFinding,
			StructuredResult: json.RawMessage(sr.StructuredResult),
		}
	}
	return out
}

// createTaskResponse is design §4.3's 202 body — deliberately thin (just
// enough for a caller to start polling GET /tasks/{id}); pr_number,
// step_runs, and the rest live there instead.
type createTaskResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// taskStatusResponse is GET /tasks/{id}'s (design §4.3) body.
type taskStatusResponse struct {
	TaskID              string        `json:"task_id"`
	Status              string        `json:"status"`
	CurrentRound        int           `json:"current_round"`
	PRNumber            int           `json:"pr_number"`
	ApprovalIssueNumber *int          `json:"approval_issue_number,omitempty"`
	Reason              string        `json:"reason,omitempty"`
	StepRuns            []stepRunWire `json:"step_runs"`
}

// server holds the wired dependencies the HTTP handlers close over.
type server struct {
	git     gitclient.GitHostClient
	secrets secretsclient.SecretsClient
	runner  *taskrunner.Runner
}

func newServeMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", s.handleCreateTask)
	mux.HandleFunc("GET /tasks/{id}", s.handleGetTask)
	mux.HandleFunc("POST /webhooks/forgejo", s.handleForgejoWebhook)
	return mux
}

func (s *server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TargetRepo == "" {
		http.Error(w, "target_repo is required", http.StatusBadRequest)
		return
	}
	if _, _, ok := strings.Cut(req.TargetRepo, "/"); !ok {
		http.Error(w, "target_repo must be \"owner/name\"", http.StatusBadRequest)
		return
	}
	if req.TaskID == "" {
		req.TaskID = fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	if req.Branch == "" {
		req.Branch = fmt.Sprintf("task/%s", req.TaskID)
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if req.RiskTier == "" {
		req.RiskTier = "low"
	}

	task := domain.Task{
		ID:                  req.TaskID,
		Type:                "new-task",
		TargetRepo:          req.TargetRepo,
		TriggeringPrincipal: req.TriggeringPrincipal,
		BotAccount:          req.BotAccount,
		RiskTier:            req.RiskTier,
		Branch:              req.Branch,
		BaseBranch:          req.BaseBranch,
		Description:         req.Description,
	}

	// design docs/designs/completed/full-pipeline-async-approval-design.md §4.3:
	// Start kicks off round 1 in the background and returns immediately —
	// the handler replies 202 without waiting for a terminal outcome.
	// GET /tasks/{id} is how a caller finds out what happened next. PR
	// creation no longer happens here (Task Intent Propagation design
	// §5) — coreloop opens it itself once the generate phase completes.
	ctx := r.Context()
	if _, _, err := s.runner.Start(ctx, task); err != nil {
		http.Error(w, "start task: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(createTaskResponse{TaskID: task.ID, Status: domain.TaskStatusRunning})
}

func (s *server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	status, ok, err := s.runner.GetTaskStatus(r.Context(), taskID)
	if err != nil {
		http.Error(w, "get task: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such task: "+taskID, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(taskStatusResponse{
		TaskID:              status.Task.ID,
		Status:              status.State.Status,
		CurrentRound:        status.State.CurrentRound,
		PRNumber:            status.Task.PRNumber,
		ApprovalIssueNumber: status.State.ApprovalIssueNumber,
		Reason:              status.State.Reason,
		StepRuns:            stepRunsToWire(status.StepRuns),
	})
}
