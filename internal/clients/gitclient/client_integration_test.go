//go:build integration

package gitclient_test

import (
	"context"
	"os"
	"testing"

	giteasdk "code.gitea.io/sdk/gitea"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

func requireForgejoEnv(t *testing.T) (url, token, owner string) {
	t.Helper()
	url = os.Getenv("FORGEJO_URL")
	token = os.Getenv("FORGEJO_ADMIN_TOKEN")
	owner = os.Getenv("FORGEJO_ADMIN_USERNAME")
	if url == "" || token == "" || owner == "" {
		t.Skip("FORGEJO_URL / FORGEJO_ADMIN_TOKEN / FORGEJO_ADMIN_USERNAME not set; source deploy/.env first")
	}
	return url, token, owner
}

func TestClient_GitHostClientSurface(t *testing.T) {
	url, token, owner := requireForgejoEnv(t)
	ctx := context.Background()

	// Set up a scratch repo with a base commit and a feature branch/commit,
	// using the raw SDK directly (test scaffolding, not the code under test).
	raw, err := giteasdk.NewClient(url, giteasdk.SetToken(token))
	if err != nil {
		t.Fatalf("raw SDK client: %v", err)
	}
	repoName := "gitclient-integration-scratch"
	repo, _, err := raw.CreateRepo(giteasdk.CreateRepoOption{Name: repoName, AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo (setup): %v", err)
	}
	t.Cleanup(func() { raw.DeleteRepo(owner, repoName) })

	_, _, err = raw.CreateFile(owner, repo.Name, "feature.txt", giteasdk.CreateFileOptions{
		FileOptions: giteasdk.FileOptions{
			Message:       "add feature",
			BranchName:    "main",
			NewBranchName: "feature-branch",
		},
		Content: "ZmVhdHVyZQ==", // base64("feature")
	})
	if err != nil {
		t.Fatalf("CreateFile (setup): %v", err)
	}

	client, err := gitclient.NewClient(url, token)
	if err != nil {
		t.Fatalf("gitclient.NewClient: %v", err)
	}
	repoRef := gitclient.RepoRef{Owner: owner, Name: repo.Name}

	t.Run("GetFileContents", func(t *testing.T) {
		got, err := client.GetFileContents(ctx, repoRef, "feature.txt", "feature-branch")
		if err != nil {
			t.Fatalf("GetFileContents: %v", err)
		}
		if string(got) != "feature" {
			t.Fatalf("GetFileContents = %q, want %q", got, "feature")
		}
	})

	var prNumber int
	t.Run("CreateOrUpdatePR creates", func(t *testing.T) {
		pr, err := client.CreateOrUpdatePR(ctx, repoRef, "feature-branch", "main", "Add feature", "initial body")
		if err != nil {
			t.Fatalf("CreateOrUpdatePR: %v", err)
		}
		if pr.Title != "Add feature" || pr.Head != "feature-branch" || pr.Base != "main" {
			t.Fatalf("unexpected PR: %+v", pr)
		}
		prNumber = pr.Number
	})

	t.Run("CreateOrReuseBranch creates a fresh branch and opening a zero-diff PR against it succeeds", func(t *testing.T) {
		branch, err := client.CreateOrReuseBranch(ctx, repoRef, "zero-diff-branch", "main")
		if err != nil {
			t.Fatalf("CreateOrReuseBranch: %v", err)
		}
		if branch.Name != "zero-diff-branch" || branch.SHA == "" {
			t.Fatalf("unexpected branch: %+v", branch)
		}

		pr, err := client.CreateOrUpdatePR(ctx, repoRef, "zero-diff-branch", "main", "Zero diff", "no commits yet")
		if err != nil {
			t.Fatalf("CreateOrUpdatePR against a zero-diff branch: %v — Forgejo may reject a head with no commits ahead of base; if so, CreateOrReuseBranch needs an empty-commit fallback (see design doc's Named Risks)", err)
		}
		if pr.Head != "zero-diff-branch" {
			t.Fatalf("unexpected PR: %+v", pr)
		}
	})

	t.Run("CreateOrReuseBranch is idempotent against an already-existing branch", func(t *testing.T) {
		first, err := client.CreateOrReuseBranch(ctx, repoRef, "zero-diff-branch", "main")
		if err != nil {
			t.Fatalf("CreateOrReuseBranch (first): %v", err)
		}
		second, err := client.CreateOrReuseBranch(ctx, repoRef, "zero-diff-branch", "main")
		if err != nil {
			t.Fatalf("CreateOrReuseBranch (second): %v", err)
		}
		if first.SHA != second.SHA {
			t.Fatalf("expected the same branch to be reused, got SHAs %q and %q", first.SHA, second.SHA)
		}
	})

	t.Run("CreateOrUpdatePR updates the existing open PR instead of duplicating it", func(t *testing.T) {
		pr, err := client.CreateOrUpdatePR(ctx, repoRef, "feature-branch", "main", "Add feature (updated)", "updated body")
		if err != nil {
			t.Fatalf("CreateOrUpdatePR (update): %v", err)
		}
		if pr.Number != prNumber {
			t.Fatalf("expected the same PR #%d to be updated, got #%d", prNumber, pr.Number)
		}
		if pr.Title != "Add feature (updated)" {
			t.Fatalf("expected title to be updated, got %q", pr.Title)
		}
	})

	t.Run("GetPR", func(t *testing.T) {
		pr, err := client.GetPR(ctx, repoRef, prNumber)
		if err != nil {
			t.Fatalf("GetPR: %v", err)
		}
		if pr.Number != prNumber {
			t.Fatalf("GetPR returned #%d, want #%d", pr.Number, prNumber)
		}
	})

	t.Run("PostPRComment and ListPRComments", func(t *testing.T) {
		if _, err := client.PostPRComment(ctx, repoRef, prNumber, "looks good"); err != nil {
			t.Fatalf("PostPRComment: %v", err)
		}
		comments, err := client.ListPRComments(ctx, repoRef, prNumber)
		if err != nil {
			t.Fatalf("ListPRComments: %v", err)
		}
		found := false
		for _, c := range comments {
			if c.Body == "looks good" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected to find posted comment, got %+v", comments)
		}
	})

	t.Run("GetBranchProtection absent case", func(t *testing.T) {
		if _, err := client.GetBranchProtection(ctx, repoRef, "main"); err == nil {
			t.Fatal("expected an error for a branch with no protection rule configured")
		}
	})

	t.Run("VerifyCollaboratorRole", func(t *testing.T) {
		ok, err := client.VerifyCollaboratorRole(ctx, repoRef, owner, gitclient.RoleRead)
		if err != nil {
			t.Fatalf("VerifyCollaboratorRole: %v", err)
		}
		if !ok {
			t.Fatal("expected the repo owner to satisfy at least RoleRead")
		}
	})

	t.Run("CreateWebhook", func(t *testing.T) {
		wh, err := client.CreateWebhook(ctx, repoRef, gitclient.WebhookConfig{
			URL:         "http://example.invalid/webhook",
			ContentType: "json",
			Events:      []string{"pull_request"},
		})
		if err != nil {
			t.Fatalf("CreateWebhook: %v", err)
		}
		if !wh.Active {
			t.Fatal("expected created webhook to be active")
		}
	})
}
