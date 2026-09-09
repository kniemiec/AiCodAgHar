package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/domain"
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
	Round        int    `json:"round"`
	StepID       string `json:"step_id"`
	StepType     string `json:"step_type"`
	InvocationID string `json:"invocation_id"`
	Blocking     bool   `json:"blocking"`
	ExitCode     int32  `json:"exit_code"`
	HasFinding   bool   `json:"has_finding"`
}

type createTaskResponse struct {
	TaskID    string        `json:"task_id"`
	PRNumber  int           `json:"pr_number"`
	Outcome   string        `json:"outcome"`
	RoundsRun int           `json:"rounds_run"`
	Reason    string        `json:"reason,omitempty"`
	StepRuns  []stepRunWire `json:"step_runs"`
}

// server holds the wired dependencies the HTTP handlers close over.
type server struct {
	git  gitclient.GitHostClient
	loop coreloop.Dependencies
}

func newServeMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", s.handleCreateTask)
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

	result, err := coreloop.Run(ctx, s.loop, task)
	if err != nil {
		http.Error(w, "run core loop: "+err.Error(), http.StatusInternalServerError)
		return
	}

	stepRuns := make([]stepRunWire, len(result.StepRuns))
	for i, sr := range result.StepRuns {
		stepRuns[i] = stepRunWire{
			Round: sr.Round, StepID: sr.StepID, StepType: sr.StepType, InvocationID: sr.InvocationID,
			Blocking: sr.Blocking, ExitCode: sr.ExitCode, HasFinding: sr.HasFinding,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(createTaskResponse{
		TaskID: task.ID, PRNumber: pr.Number, Outcome: string(result.Outcome),
		RoundsRun: result.RoundsRun, Reason: result.Reason, StepRuns: stepRuns,
	})
}
