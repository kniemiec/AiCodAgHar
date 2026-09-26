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

// Branch is the subset of a Forgejo branch the orchestrator needs.
type Branch struct {
	Name string
	SHA  string
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

// Issue is the subset of a Forgejo issue the orchestrator needs — design
// §4.4's approval issue.
type Issue struct {
	Number int
	Title  string
	Body   string
}

// GitHostClient is ADR-9's Forgejo test-seam interface. Its production
// implementation wraps code.gitea.io/sdk/gitea (confirmed to cover this
// surface by the Phase 0 spike, internal/clients/gitclient/spike_test.go);
// its server side in this plan is the real seeded Forgejo instance itself —
// there is no gitclient-mock.
type GitHostClient interface {
	GetFileContents(ctx context.Context, repo RepoRef, path, ref string) ([]byte, error)

	// CreateOrReuseBranch creates a branch off base if it doesn't already
	// exist, or returns the existing branch as-is — idempotent against a
	// crash between creating the branch and starting the task, which would
	// otherwise error on retry instead of proceeding.
	CreateOrReuseBranch(ctx context.Context, repo RepoRef, branch, base string) (Branch, error)

	CreateOrUpdatePR(ctx context.Context, repo RepoRef, branch, base, title, body string) (PullRequest, error)
	GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error)
	PostPRComment(ctx context.Context, repo RepoRef, number int, body string) (Comment, error)
	ListPRComments(ctx context.Context, repo RepoRef, number int) ([]Comment, error)
	GetBranchProtection(ctx context.Context, repo RepoRef, branch string) (BranchProtection, error)
	VerifyCollaboratorRole(ctx context.Context, repo RepoRef, username string, minRole Role) (bool, error)
	CreateWebhook(ctx context.Context, repo RepoRef, cfg WebhookConfig) (Webhook, error)

	// CreateOrReuseIssue opens the approval issue for a repo, or returns the
	// existing open one with the same title if one already exists (design
	// §4.4) — idempotent against a crash between opening the issue and
	// persisting its number, which would otherwise duplicate it on retry.
	CreateOrReuseIssue(ctx context.Context, repo RepoRef, title, body string) (Issue, error)

	// ListIssueLabels independently re-reads an issue's current labels —
	// design §4.4's webhook handler never trusts a webhook payload's own
	// claim about what labels are present.
	ListIssueLabels(ctx context.Context, repo RepoRef, issueNumber int) ([]string, error)
}
