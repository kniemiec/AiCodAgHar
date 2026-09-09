//go:build spike

package gitclient

import (
	"context"
	"os"
	"testing"

	"code.gitea.io/sdk/gitea"
)

// Throwaway exploratory test: confirms code.gitea.io/sdk/gitea covers every
// GitHostClient call the plan names, run against a live local Forgejo.
// Not an assertion of production behavior — Phase 3 owns that.
func TestSDKCoversGitHostClientSurface(t *testing.T) {
	url := os.Getenv("FORGEJO_URL")
	token := os.Getenv("FORGEJO_ADMIN_TOKEN")
	if url == "" || token == "" {
		t.Skip("FORGEJO_URL / FORGEJO_ADMIN_TOKEN not set; source deploy/.env first")
	}

	client, err := gitea.NewClient(url, gitea.SetToken(token))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	owner := os.Getenv("FORGEJO_ADMIN_USERNAME")
	if owner == "" {
		t.Skip("FORGEJO_ADMIN_USERNAME not set; source deploy/.env first")
	}
	repoName := "spike-scratch-repo"

	repo, _, err := client.CreateRepo(gitea.CreateRepoOption{Name: repoName, AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo (setup): %v", err)
	}
	t.Cleanup(func() { client.DeleteRepo(owner, repoName) })

	t.Run("GetFileContents", func(t *testing.T) {
		if _, _, err := client.GetContents(owner, repo.Name, "main", "README.md"); err != nil {
			t.Errorf("GetContents: %v", err)
		}
	})

	t.Run("CreateOrUpdatePR_via_CreateFile+CreatePullRequest", func(t *testing.T) {
		_, _, err := client.CreateFile(owner, repo.Name, "spike.txt", gitea.CreateFileOptions{
			FileOptions: gitea.FileOptions{
				Message:       "spike commit",
				BranchName:    "main",
				NewBranchName: "spike-branch",
			},
			Content: "c3Bpa2U=", // base64("spike")
		})
		if err != nil {
			t.Fatalf("CreateFile (branch+commit): %v", err)
		}
		pr, _, err := client.CreatePullRequest(owner, repo.Name, gitea.CreatePullRequestOption{
			Head: "spike-branch", Base: "main", Title: "spike PR",
		})
		if err != nil {
			t.Fatalf("CreatePullRequest: %v", err)
		}

		t.Run("GetPR", func(t *testing.T) {
			if _, _, err := client.GetPullRequest(owner, repo.Name, pr.Index); err != nil {
				t.Errorf("GetPullRequest: %v", err)
			}
		})
		t.Run("PostPRComment", func(t *testing.T) {
			if _, _, err := client.CreateIssueComment(owner, repo.Name, pr.Index, gitea.CreateIssueCommentOption{Body: "spike comment"}); err != nil {
				t.Errorf("CreateIssueComment: %v", err)
			}
		})
		t.Run("ListPRComments", func(t *testing.T) {
			if _, _, err := client.ListIssueComments(owner, repo.Name, pr.Index, gitea.ListIssueCommentOptions{}); err != nil {
				t.Errorf("ListIssueComments: %v", err)
			}
		})
	})

	t.Run("GetBranchProtection_absent_case", func(t *testing.T) {
		_, resp, err := client.GetBranchProtection(owner, repo.Name, "main")
		if err != nil && (resp == nil || resp.StatusCode != 404) {
			t.Errorf("GetBranchProtection unexpected error: %v", err)
		}
	})

	t.Run("VerifyCollaboratorRole_via_CollaboratorPermission", func(t *testing.T) {
		if _, _, err := client.CollaboratorPermission(owner, repo.Name, owner); err != nil {
			t.Errorf("CollaboratorPermission: %v", err)
		}
	})

	t.Run("CreateWebhook", func(t *testing.T) {
		_, _, err := client.CreateRepoHook(owner, repo.Name, gitea.CreateHookOption{
			Type:   "gitea",
			Config: map[string]string{"url": "http://example.invalid/webhook", "content_type": "json"},
			Active: true,
		})
		if err != nil {
			t.Errorf("CreateRepoHook: %v", err)
		}
	})

	_ = context.Background() // reserved if a context-aware client variant replaces this later
}
