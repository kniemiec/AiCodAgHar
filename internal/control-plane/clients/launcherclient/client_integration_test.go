//go:build integration

package launcherclient_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	giteasdk "code.gitea.io/sdk/gitea"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; source deploy/.env and export TLS_* vars first", name)
	}
	return v
}

func newTestClient(t *testing.T) *launcherclient.Client {
	t.Helper()
	addr := requireEnv(t, "LAUNCHER_MOCK_ADDR")
	tlsCfg := launcherclient.TLSConfig{
		CACertPath:     requireEnv(t, "TLS_CA_CERT"),
		ClientCertPath: requireEnv(t, "TLS_CLIENT_CERT"),
		ClientKeyPath:  requireEnv(t, "TLS_CLIENT_KEY"),
	}
	client, err := launcherclient.NewClient(addr, tlsCfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestClient_NoopSuccess(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	result, err := client.Launch(ctx, launcherclient.LaunchRequest{
		InvocationID: "inv-noop-1",
		TaskID:       "task-1",
		StepType:     "agent-stage",
		StepID:       "review",
		PayloadRef:   "noop-success",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.ExitCode != 0 || result.Status != "completed" {
		t.Fatalf("unexpected result: %+v", result)
	}

	logs, err := client.ExtractLogs(ctx, result.InvocationID)
	if err != nil {
		t.Fatalf("ExtractLogs: %v", err)
	}
	var parsed struct {
		HasBlockingFinding bool `json:"has_blocking_finding"`
	}
	if err := json.Unmarshal(logs.StructuredResultJSON, &parsed); err != nil {
		t.Fatalf("parse structured result: %v", err)
	}
	if parsed.HasBlockingFinding {
		t.Fatal("expected no blocking finding from the noop-success fixture")
	}
}

func TestClient_DeterministicFail(t *testing.T) {
	client := newTestClient(t)

	result, err := client.Launch(context.Background(), launcherclient.LaunchRequest{
		InvocationID: "inv-fail-1",
		TaskID:       "task-1",
		StepType:     "deterministic-step",
		StepID:       "unit-tests",
		PayloadRef:   "deterministic-fail",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", result.ExitCode)
	}
}

func TestClient_UnknownPayloadRefFails(t *testing.T) {
	client := newTestClient(t)

	_, err := client.Launch(context.Background(), launcherclient.LaunchRequest{
		InvocationID: "inv-unknown-1",
		TaskID:       "task-1",
		StepType:     "agent-stage",
		StepID:       "review",
		PayloadRef:   "no-such-fixture",
	})
	if err == nil {
		t.Fatal("expected an error for an unknown payload_ref")
	}
}

// TestClient_PostPRCommentUsesInjectedCredential proves the credential
// round-trip: launcher-mock authenticates to Forgejo using the token this
// test injects via env, not any credential of its own, and the resulting
// comment is really visible via the GitHostClient production client.
func TestClient_PostPRCommentUsesInjectedCredential(t *testing.T) {
	client := newTestClient(t)

	forgejoURL := requireEnv(t, "FORGEJO_URL")
	// launcher-mock runs inside the compose network, so the credential it's
	// handed must point at Forgejo's in-network address, not the host-side
	// FORGEJO_URL this test itself uses to set up/verify via the host port.
	forgejoInternalURL := requireEnv(t, "FORGEJO_INTERNAL_URL")
	forgejoToken := requireEnv(t, "FORGEJO_ADMIN_TOKEN")
	owner := requireEnv(t, "FORGEJO_ADMIN_USERNAME")

	raw, err := giteasdk.NewClient(forgejoURL, giteasdk.SetToken(forgejoToken))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}
	repoName := "launcherclient-integration-scratch"
	repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo (setup): %v", err)
	}
	t.Cleanup(func() { raw.DeleteRepo(owner, repoName) })

	_, _, err = raw.CreateFile(owner, repo.Name, "feature.txt", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{Message: "add feature", BranchName: "main", NewBranchName: "feature-branch"},
		Content:     "ZmVhdHVyZQ==",
	})
	if err != nil {
		t.Fatalf("CreateFile (setup): %v", err)
	}
	pr, _, err := raw.CreatePullRequest(owner, repo.Name, giteasdk.CreatePullRequestOption{Head: "feature-branch", Base: "main", Title: "test PR"})
	if err != nil {
		t.Fatalf("CreatePullRequest (setup): %v", err)
	}

	result, err := client.Launch(context.Background(), launcherclient.LaunchRequest{
		InvocationID: "inv-comment-1",
		TaskID:       "task-1",
		StepType:     "agent-stage",
		StepID:       "review",
		PayloadRef:   "post-blocking-finding-comment",
		Env: map[string]string{
			"FORGEJO_URL":   forgejoInternalURL,
			"FORGEJO_TOKEN": forgejoToken,
			"TARGET_REPO":   owner + "/" + repo.Name,
			"PR_NUMBER":     strconv.FormatInt(pr.Index, 10),
			"COMMENT_BODY":  "posted by launcher-mock integration test",
		},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}

	logs, err := client.ExtractLogs(context.Background(), result.InvocationID)
	if err != nil {
		t.Fatalf("ExtractLogs: %v", err)
	}
	var parsed struct {
		HasBlockingFinding bool `json:"has_blocking_finding"`
	}
	if err := json.Unmarshal(logs.StructuredResultJSON, &parsed); err != nil {
		t.Fatalf("parse structured result: %v", err)
	}
	if !parsed.HasBlockingFinding {
		t.Fatal("expected a blocking finding from the post-blocking-finding-comment fixture")
	}

	comments, _, err := raw.ListIssueComments(owner, repo.Name, pr.Index, giteasdk.ListIssueCommentOptions{})
	if err != nil {
		t.Fatalf("ListIssueComments (verify): %v", err)
	}
	found := false
	for _, c := range comments {
		if c.Body == "posted by launcher-mock integration test" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected launcher-mock to have posted the comment via the injected credential")
	}
}
