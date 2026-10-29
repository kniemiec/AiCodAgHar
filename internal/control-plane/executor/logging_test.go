package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/fakes"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// TestStepLogs_CarryIDsAndNoSecrets covers LG-2 and LG-6 for the executor
// path at debug level: every line carries the correlation fields without any
// call site repeating them, and neither the minted token nor the task
// description appears anywhere.
func TestStepLogs_CarryIDsAndNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "json")
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(&buf, "control-plane")))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const desc = "TOP-SECRET-TASK-DESCRIPTION"
	secrets := &fakes.FakeSecretsClient{}
	launcher := &fakes.FakeContainerLauncher{
		ExtractLogsFunc: func(context.Context, string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{StructuredResultJSON: []byte(`{"has_blocking_finding": false}`)}, nil
		},
	}
	e := &executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: &fakes.FakeAuditWriter{}}
	_, err := e.Execute(context.Background(), executor.StepContext{
		Task:  domain.Task{ID: "task-1", TargetRepo: "owner/repo", Description: desc},
		Round: 2,
		Step:  policyclient.ResolvedStep{StepID: "review", Type: executor.StepTypeAgentStage, BotAccount: "review-bot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := launcher.Launches()[0].Env["FORGEJO_TOKEN"]
	if token == "" {
		t.Fatal("no token minted")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected several log lines, got %q", buf.String())
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		if rec["task_id"] != "task-1" || rec["round"] != float64(2) || rec["step_id"] != "review" || rec["invocation_id"] == nil {
			t.Errorf("missing correlation fields: %s", line)
		}
	}
	for _, forbidden := range []string{token, desc} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("logs contain %q", forbidden)
		}
	}
}
