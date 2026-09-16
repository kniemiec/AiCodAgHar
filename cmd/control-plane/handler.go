package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
	"github.com/kniemiec/AiCodAgHar/internal/taskrunner"
)

type createTaskRequest struct {
	TaskID              string `json:"task_id"`
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
	if req.TargetRepo == "" || req.Branch == "" || req.TaskType == "" {
		http.Error(w, "target_repo, branch, and task_type are required", http.StatusBadRequest)
		return
	}
	if req.TaskID == "" {
		req.TaskID = fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if req.RiskTier == "" {
		req.RiskTier = "low"
	}
	if req.PRTitle == "" {
		req.PRTitle = fmt.Sprintf("[%s] %s", req.TaskType, req.Branch)
	}

	owner, name, ok := strings.Cut(req.TargetRepo, "/")
	if !ok {
		http.Error(w, "target_repo must be \"owner/name\"", http.StatusBadRequest)
		return
	}
	repo := gitclient.RepoRef{Owner: owner, Name: name}

	ctx := r.Context()
	pr, err := s.git.CreateOrUpdatePR(ctx, repo, req.Branch, req.BaseBranch, req.PRTitle, req.PRBody)
	if err != nil {
		http.Error(w, "open review PR: "+err.Error(), http.StatusBadGateway)
		return
	}

	task := domain.Task{
		ID:                  req.TaskID,
		Type:                req.TaskType,
		TargetRepo:          req.TargetRepo,
		TriggeringPrincipal: req.TriggeringPrincipal,
		BotAccount:          req.BotAccount,
		RiskTier:            req.RiskTier,
		Branch:              req.Branch,
		BaseBranch:          req.BaseBranch,
		PRNumber:            pr.Number,
	}

	// design docs/designs/next/full-pipeline-async-approval-design.md §4.3:
	// Start kicks off round 1 in the background and returns immediately —
	// the handler replies 202 without waiting for a terminal outcome.
	// GET /tasks/{id} is how a caller finds out what happened next.
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
