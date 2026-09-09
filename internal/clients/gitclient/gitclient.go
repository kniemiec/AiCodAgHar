// Package gitclient holds the GitHostClient interface; its production
// implementation over code.gitea.io/sdk/gitea lands in Phase 3.
package gitclient

import "context"

// RepoRef identifies a Forgejo repository by owner and name.
type RepoRef struct {
	Owner string
	Name  string
}

// Role is a Forgejo collaborator permission level, used by
// VerifyCollaboratorRole's minRole threshold check.
type Role string

const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleAdmin Role = "admin"
)

// PullRequest is the subset of a Forgejo pull request the orchestrator needs.
type PullRequest struct {
	Number int
	Title  string
	Body   string
	Head   string
	Base   string
	State  string
	URL    string
}

// Comment is a Forgejo issue/PR comment.
type Comment struct {
	ID   int64
	Body string
}

// BranchProtection is the subset of a Forgejo branch protection rule the
// orchestrator needs to reason about merge gating.
type BranchProtection struct {
	BranchName          string
	RequiredApprovals   int
	RequireStatusChecks bool
}

// WebhookConfig describes a webhook to register on a repository.
type WebhookConfig struct {
	URL         string
	ContentType string
	Secret      string
	Events      []string
}

// Webhook is a created Forgejo webhook.
type Webhook struct {
	ID     int64
	Active bool
}

// GitHostClient is ADR-9's Forgejo test-seam interface. Its production
// implementation wraps code.gitea.io/sdk/gitea (confirmed to cover this
// surface by the Phase 0 spike, internal/clients/gitclient/spike_test.go);
// its server side in this plan is the real seeded Forgejo instance itself —
// there is no gitclient-mock.
type GitHostClient interface {
	GetFileContents(ctx context.Context, repo RepoRef, path, ref string) ([]byte, error)
	CreateOrUpdatePR(ctx context.Context, repo RepoRef, branch, base, title, body string) (PullRequest, error)
	GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error)
	PostPRComment(ctx context.Context, repo RepoRef, number int, body string) (Comment, error)
	ListPRComments(ctx context.Context, repo RepoRef, number int) ([]Comment, error)
	GetBranchProtection(ctx context.Context, repo RepoRef, branch string) (BranchProtection, error)
	VerifyCollaboratorRole(ctx context.Context, repo RepoRef, username string, minRole Role) (bool, error)
	CreateWebhook(ctx context.Context, repo RepoRef, cfg WebhookConfig) (Webhook, error)
}
