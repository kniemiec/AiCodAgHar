package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
)

// newTestServer wires a server against fakes only, mirroring
// internal/control-plane/taskrunner/taskrunner_test.go's newDeps — no live dependencies.
func newTestServer(t *testing.T) *server {
	t.Helper()
	return newTestServerWithPolicy(t, func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		return policyclient.PolicyDecision{
			Decision: "allow",
			ResolvedSteps: []policyclient.ResolvedStep{
				{StepID: "implement", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate, Blocking: false},
				{StepID: "review", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: false},
			},
		}, nil
	})
}

func newTestServerWithPolicy(t *testing.T, evaluate func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error)) *server {
	t.Helper()
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{EvaluateFunc: evaluate}
	secrets := &fakes.FakeSecretsClient{}
	auditWriter := &fakes.FakeAuditWriter{}
	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter},
	)
	store := &fakes.FakeTaskStore{}
	git := &fakes.FakeGitHostClient{}
	runner := taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy: policy, Approval: &fakes.FakeApprovalVerifier{}, Audit: auditWriter, Executors: registry,
			PRCreator: git,
		},
		Store:    store,
		Launcher: launcher,
	})
	return &server{git: git, runner: runner}
}

// pollUntilTerminal polls GET /tasks/{id} until status leaves "running", or
// fails the test after a generous timeout — the fakes resolve in-process
// with no I/O, so a terminal status shows up within milliseconds.
func pollUntilTerminal(t *testing.T, mux http.Handler, taskID string) taskStatusResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /tasks/%s: unexpected status %d: %s", taskID, rec.Code, rec.Body.String())
		}
		var status taskStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode GET /tasks/%s response: %v", taskID, err)
		}
		if status.Status != domain.TaskStatusRunning {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %s did not reach a terminal status within the deadline", taskID)
	return taskStatusResponse{}
}

func TestHandleCreateTask_Returns202Immediately(t *testing.T) {
	s := newTestServer(t)
	mux := newServeMux(s)

	body, _ := json.Marshal(createTaskRequest{
		TargetRepo: "acme/widgets", Branch: "feature", BaseBranch: "main", Description: "implement the widget feature",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(body))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp createTaskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TaskID == "" {
		t.Fatal("expected a non-empty task_id")
	}
	if resp.Status != domain.TaskStatusRunning {
		t.Fatalf("expected status %q, got %q", domain.TaskStatusRunning, resp.Status)
	}

	// The 202 body carries none of the eventual outcome — that's GET
	// /tasks/{id}'s job (design §4.3).
	status := pollUntilTerminal(t, mux, resp.TaskID)
	if status.Status != domain.TaskStatusStopSuccess {
		t.Fatalf("expected the task to reach stop_success, got %q (reason: %s)", status.Status, status.Reason)
	}
	if status.PRNumber == 0 {
		t.Fatal("expected a non-zero pr_number once the task is queryable")
	}
	if len(status.StepRuns) == 0 {
		t.Fatal("expected at least one step_run once the task is queryable")
	}
}

// TestHandleCreateTask_BranchOptional exercises Task Intent Propagation
// design §5: a task no longer arrives with a branch already pushed, so
// omitting branch entirely must still succeed, with the server generating
// one and opening the review PR against it once the generate phase runs.
func TestHandleCreateTask_BranchOptional(t *testing.T) {
	s := newTestServer(t)
	mux := newServeMux(s)

	body, _ := json.Marshal(createTaskRequest{TargetRepo: "acme/widgets", Description: "implement the widget feature"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(body))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp createTaskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	status := pollUntilTerminal(t, mux, resp.TaskID)
	if status.Status != domain.TaskStatusStopSuccess {
		t.Fatalf("expected the task to reach stop_success, got %q (reason: %s)", status.Status, status.Reason)
	}
	if status.PRNumber == 0 {
		t.Fatal("expected a non-zero pr_number once the generate phase completes")
	}
}

func TestHandleGetTask_UnknownTaskIs404(t *testing.T) {
	s := newTestServer(t)
	mux := newServeMux(s)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tasks/no-such-task", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func postJSON(mux http.Handler, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return rec
}

func getJSON(mux http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// failingPolicyServer's PDP errors until *healthy is flipped, so a task fails
// retryably (dependency/pdp) and a later admin retry can succeed.
func failingPolicyServer(t *testing.T, healthy *atomic.Bool) *server {
	return newTestServerWithPolicy(t, func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		if !healthy.Load() {
			return policyclient.PolicyDecision{}, errors.New("pdp unreachable")
		}
		return policyclient.PolicyDecision{Decision: "allow", ResolvedSteps: []policyclient.ResolvedStep{
			{StepID: "review", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview},
		}}, nil
	})
}

func TestFailedTaskStatusAndAdminRetry(t *testing.T) {
	var healthy atomic.Bool
	s := failingPolicyServer(t, &healthy)
	mux, admin := newServeMux(s), newAdminMux(s)

	rec := postJSON(mux, "/tasks", createTaskRequest{TaskID: "t-1", TargetRepo: "acme/widgets"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	status := pollUntilTerminal(t, mux, "t-1")
	if status.Status != domain.TaskStatusFailed || status.ErrorClass != "dependency" || status.Retryable == nil || !*status.Retryable {
		t.Fatalf("status = %+v", status)
	}
	if status.AcceptedAt == nil || status.FinishedAt != nil {
		t.Fatalf("accepted_at/finished_at = %v/%v, want set/unset for a retryable failure", status.AcceptedAt, status.FinishedAt)
	}

	// Admin listing sees it.
	var listed []adminTaskWire
	rec = getJSON(admin, "/admin/tasks?status=failed")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &listed) != nil || len(listed) != 1 || listed[0].TaskID != "t-1" {
		t.Fatalf("admin list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := getJSON(admin, "/admin/tasks?status=bogus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status = %d", rec.Code)
	}

	// Retry validation and 404.
	if rec := postJSON(admin, "/admin/tasks/t-1/retry", retryRequest{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing operator = %d", rec.Code)
	}
	if rec := postJSON(admin, "/admin/tasks/nope/retry", retryRequest{Operator: "alice"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task = %d", rec.Code)
	}

	healthy.Store(true)
	if rec := postJSON(admin, "/admin/tasks/t-1/retry", retryRequest{Operator: "alice"}); rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	status = pollUntilTerminal(t, mux, "t-1")
	if status.Status != domain.TaskStatusStopSuccess || status.RetryCount != 1 || status.FinishedAt == nil {
		t.Fatalf("after retry: %+v", status)
	}
	// Finished now: a second retry conflicts.
	if rec := postJSON(admin, "/admin/tasks/t-1/retry", retryRequest{Operator: "alice"}); rec.Code != http.StatusConflict {
		t.Fatalf("retry of finished task = %d", rec.Code)
	}

	// The public mux does not serve admin routes.
	if rec := postJSON(mux, "/admin/tasks/t-1/retry", retryRequest{Operator: "alice"}); rec.Code != http.StatusNotFound {
		t.Fatalf("admin route on public mux = %d", rec.Code)
	}
}

func TestGetTaskEvents(t *testing.T) {
	s := newTestServer(t)
	mux := newServeMux(s)
	postJSON(mux, "/tasks", createTaskRequest{TaskID: "t-1", TargetRepo: "acme/widgets"})
	pollUntilTerminal(t, mux, "t-1")

	var all eventsResponse
	rec := getJSON(mux, "/tasks/t-1/events")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &all) != nil {
		t.Fatalf("events: %d %s", rec.Code, rec.Body.String())
	}
	if len(all.Events) < 5 || all.Events[0].Type != domain.EventTaskAccepted || all.Events[len(all.Events)-1].Type != domain.EventTaskFinished {
		t.Fatalf("expected accepted ... finished, got %d events", len(all.Events))
	}
	if all.NextAfterID != all.Events[len(all.Events)-1].ID {
		t.Fatalf("next_after_id = %d", all.NextAfterID)
	}

	// Pagination: limit=2 then continue after the cursor; pages concatenate to the whole.
	var got []eventWire
	after := int64(0)
	for i := 0; i < 50; i++ {
		var page eventsResponse
		rec := getJSON(mux, fmt.Sprintf("/tasks/t-1/events?limit=2&after_id=%d", after))
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
			t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
		}
		if len(page.Events) == 0 {
			break
		}
		got = append(got, page.Events...)
		after = page.NextAfterID
	}
	if len(got) != len(all.Events) {
		t.Fatalf("paged %d events, want %d", len(got), len(all.Events))
	}

	// Type filter.
	var steps eventsResponse
	rec = getJSON(mux, "/tasks/t-1/events?type=step.resolved")
	json.Unmarshal(rec.Body.Bytes(), &steps)
	if len(steps.Events) != 2 {
		t.Fatalf("step.resolved events = %d, want 2", len(steps.Events))
	}

	for _, bad := range []string{"since=yesterday", "limit=0", "after_id=-1", "until=x"} {
		if rec := getJSON(mux, "/tasks/t-1/events?"+bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", bad, rec.Code)
		}
	}
	if rec := getJSON(mux, "/tasks/nope/events"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task = %d", rec.Code)
	}
}
