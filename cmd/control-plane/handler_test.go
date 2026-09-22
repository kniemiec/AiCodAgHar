package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	launcher := &fakes.FakeContainerLauncher{}
	policy := &fakes.FakePolicyClient{
		EvaluateFunc: func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
			return policyclient.PolicyDecision{
				Decision: "allow",
				ResolvedSteps: []policyclient.ResolvedStep{
					{StepID: "implement", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseGenerate, Blocking: false},
					{StepID: "review", Type: executor.StepTypeAgentStage, Phase: policyclient.PhaseReview, Blocking: false},
				},
			}, nil
		},
	}
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
