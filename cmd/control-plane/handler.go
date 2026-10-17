package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/instrumented"
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

	// Additive (platform-observability design §3): timing and failure state.
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ErrorClass string     `json:"error_class,omitempty"`
	Retryable  *bool      `json:"retryable,omitempty"`
	RetryCount int        `json:"retry_count,omitempty"`
}

// server holds the wired dependencies the HTTP handlers close over.
type server struct {
	git     gitclient.GitHostClient
	secrets secretsclient.SecretsClient
	runner  *taskrunner.Runner
	metrics *instrumented.Metrics // nil-safe
}

func newServeMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", s.handleCreateTask)
	mux.HandleFunc("GET /tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /tasks/{id}/events", s.handleGetTaskEvents)
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

	resp := taskStatusResponse{
		TaskID:              status.Task.ID,
		Status:              status.State.Status,
		CurrentRound:        status.State.CurrentRound,
		PRNumber:            status.Task.PRNumber,
		ApprovalIssueNumber: status.State.ApprovalIssueNumber,
		Reason:              status.State.Reason,
		StepRuns:            stepRunsToWire(status.StepRuns),
		FinishedAt:          status.State.FinishedAt,
		ErrorClass:          status.State.ErrorClass,
		RetryCount:          status.State.RetryCount,
	}
	if !status.State.AcceptedAt.IsZero() {
		t := status.State.AcceptedAt
		resp.AcceptedAt = &t
	}
	if status.State.Status == domain.TaskStatusFailed {
		retryable := status.State.Retryable
		resp.Retryable = &retryable
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

const (
	defaultEventsLimit = 200
	maxEventsLimit     = 1000
)

type eventWire struct {
	ID           int64          `json:"id"`
	At           time.Time      `json:"at"`
	Round        int            `json:"round"`
	StepID       string         `json:"step_id,omitempty"`
	InvocationID string         `json:"invocation_id,omitempty"`
	Type         string         `json:"type"`
	Payload      map[string]any `json:"payload"`
	TraceID      string         `json:"trace_id,omitempty"`
	SpanID       string         `json:"span_id,omitempty"`
}

type eventsResponse struct {
	Events      []eventWire `json:"events"`
	NextAfterID int64       `json:"next_after_id"`
}

// handleGetTaskEvents serves GET /tasks/{id}/events?type=&since=&until=&after_id=&limit=
// — a task's append-only timeline in id order (design §3). since/until are
// RFC 3339; pass the previous response's next_after_id as after_id to page.
func (s *server) handleGetTaskEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := domain.EventFilter{Type: q.Get("type"), Limit: defaultEventsLimit}
	var err error
	if v := q.Get("since"); v != "" {
		if f.Since, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "since must be RFC 3339", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("until"); v != "" {
		if f.Until, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "until must be RFC 3339", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("after_id"); v != "" {
		if f.AfterID, err = strconv.ParseInt(v, 10, 64); err != nil || f.AfterID < 0 {
			http.Error(w, "after_id must be a non-negative integer", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		f.Limit = min(n, maxEventsLimit)
	}

	taskID := r.PathValue("id")
	events, ok, err := s.runner.TaskEvents(r.Context(), taskID, f)
	if err != nil {
		http.Error(w, "get events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such task: "+taskID, http.StatusNotFound)
		return
	}

	resp := eventsResponse{Events: make([]eventWire, len(events)), NextAfterID: f.AfterID}
	for i, e := range events {
		payload := e.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		resp.Events[i] = eventWire{ID: e.ID, At: e.At, Round: e.Round, StepID: e.StepID, InvocationID: e.InvocationID,
			Type: e.Type, Payload: payload, TraceID: e.TraceID, SpanID: e.SpanID}
		resp.NextAfterID = e.ID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// newAdminMux is the admin listener's routes (ADMIN_ADDR, loopback by
// default): read-only except retry. There is no authentication yet (N4); the
// separate listener is containment, not a substitute.
func newAdminMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/tasks", s.handleAdminListTasks)
	mux.HandleFunc("POST /admin/tasks/{id}/retry", s.handleAdminRetry)
	return mux
}

type adminTaskWire struct {
	TaskID     string    `json:"task_id"`
	Status     string    `json:"status"`
	Since      time.Time `json:"since"`
	AgeSeconds int64     `json:"age_seconds"`
}

func (s *server) handleAdminListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case domain.TaskStatusRunning, domain.TaskStatusWaitingApproval, domain.TaskStatusStopSuccess,
		domain.TaskStatusStopEscalate, domain.TaskStatusFailed:
	default:
		http.Error(w, "status must be one of running, waiting_approval, stop_success, stop_escalate, failed", http.StatusBadRequest)
		return
	}
	tasks, err := s.runner.TasksByStatus(r.Context(), status)
	if err != nil {
		http.Error(w, "list tasks: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	out := make([]adminTaskWire, len(tasks))
	for i, t := range tasks {
		out[i] = adminTaskWire{TaskID: t.TaskID, Status: t.Status, Since: t.Since, AgeSeconds: int64(now.Sub(t.Since).Seconds())}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type retryRequest struct {
	Operator string `json:"operator"`
}

const maxOperatorBytes = 64

// handleAdminRetry serves POST /admin/tasks/{id}/retry (TS-9): 202 once the
// compare-and-set succeeded (the continuation runs in the background — poll
// GET /tasks/{id}), 409 if the task isn't a retryable failed task with
// retries left, 404 if unknown. operator is self-declared and recorded with
// the remote address; it is not authentication.
func (s *server) handleAdminRetry(w http.ResponseWriter, r *http.Request) {
	var req retryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Operator == "" || len(req.Operator) > maxOperatorBytes {
		http.Error(w, fmt.Sprintf("operator is required (1-%d bytes)", maxOperatorBytes), http.StatusBadRequest)
		return
	}

	taskID := r.PathValue("id")
	if _, err := s.runner.StartRetry(r.Context(), taskID, req.Operator, r.RemoteAddr); err != nil {
		switch {
		case errors.Is(err, taskrunner.ErrNoSuchTask):
			http.Error(w, "no such task: "+taskID, http.StatusNotFound)
		case errors.Is(err, taskrunner.ErrNotRetryable):
			http.Error(w, "task is not retryable", http.StatusConflict)
		default:
			http.Error(w, "retry task: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(createTaskResponse{TaskID: taskID, Status: domain.TaskStatusRunning})
}
