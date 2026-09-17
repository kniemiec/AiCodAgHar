package gitclient

import (
	"context"
	"encoding/base64"
	"fmt"

	"code.gitea.io/sdk/gitea"
)

// Client is GitHostClient's production implementation, wrapping
// code.gitea.io/sdk/gitea (confirmed to cover this surface by the Phase 0
// spike, spike_test.go). The underlying SDK is not context-aware; ctx is
// honored only as an early cancellation check.
type Client struct {
	sdk *gitea.Client
}

// NewClient builds a Client authenticated against a Forgejo instance at
// baseURL with a personal access token.
func NewClient(baseURL, token string) (*Client, error) {
	sdk, err := gitea.NewClient(baseURL, gitea.SetToken(token))
	if err != nil {
		return nil, fmt.Errorf("gitclient: new SDK client: %w", err)
	}
	return &Client{sdk: sdk}, nil
}

func (c *Client) GetFileContents(ctx context.Context, repo RepoRef, path, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, _, err := c.sdk.GetContents(repo.Owner, repo.Name, ref, path)
	if err != nil {
		return nil, fmt.Errorf("gitclient: get contents %s@%s/%s: %w", repo.Owner+"/"+repo.Name, ref, path, err)
	}
	if content.Content == nil {
		return nil, fmt.Errorf("gitclient: %s@%s/%s is not a file (type=%s)", repo.Owner+"/"+repo.Name, ref, path, content.Type)
	}
	decoded, err := base64.StdEncoding.DecodeString(*content.Content)
	if err != nil {
		return nil, fmt.Errorf("gitclient: decode contents of %s@%s/%s: %w", repo.Owner+"/"+repo.Name, ref, path, err)
	}
	return decoded, nil
}

// CreateOrUpdatePR opens a PR for branch -> base, or updates title/body if
// an open PR from that branch already exists. It does not push any commits
// itself — the branch is expected to already carry the changes (pushed by
// whatever executed the step).
func (c *Client) CreateOrUpdatePR(ctx context.Context, repo RepoRef, branch, base, title, body string) (PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return PullRequest{}, err
	}

	existing, err := c.findOpenPRByHead(repo, branch)
	if err != nil {
		return PullRequest{}, err
	}
	if existing != nil {
		updated, _, err := c.sdk.EditPullRequest(repo.Owner, repo.Name, existing.Index, gitea.EditPullRequestOption{
			Title: title,
			Body:  &body,
		})
		if err != nil {
			return PullRequest{}, fmt.Errorf("gitclient: edit PR #%d: %w", existing.Index, err)
		}
		return toPullRequest(updated), nil
	}

	created, _, err := c.sdk.CreatePullRequest(repo.Owner, repo.Name, gitea.CreatePullRequestOption{
		Head:  branch,
		Base:  base,
		Title: title,
		Body:  body,
	})
	if err != nil {
		return PullRequest{}, fmt.Errorf("gitclient: create PR %s -> %s: %w", branch, base, err)
	}
	return toPullRequest(created), nil
}

func (c *Client) findOpenPRByHead(repo RepoRef, branch string) (*gitea.PullRequest, error) {
	prs, _, err := c.sdk.ListRepoPullRequests(repo.Owner, repo.Name, gitea.ListPullRequestsOptions{
		State: gitea.StateOpen,
	})
	if err != nil {
		return nil, fmt.Errorf("gitclient: list open PRs for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	for _, pr := range prs {
		if pr.Head != nil && pr.Head.Ref == branch {
			return pr, nil
		}
	}
	return nil, nil
}

func (c *Client) GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return PullRequest{}, err
	}
	pr, _, err := c.sdk.GetPullRequest(repo.Owner, repo.Name, int64(number))
	if err != nil {
		return PullRequest{}, fmt.Errorf("gitclient: get PR #%d: %w", number, err)
	}
	return toPullRequest(pr), nil
}

func (c *Client) PostPRComment(ctx context.Context, repo RepoRef, number int, body string) (Comment, error) {
	if err := ctx.Err(); err != nil {
		return Comment{}, err
	}
	comment, _, err := c.sdk.CreateIssueComment(repo.Owner, repo.Name, int64(number), gitea.CreateIssueCommentOption{Body: body})
	if err != nil {
		return Comment{}, fmt.Errorf("gitclient: post comment on PR #%d: %w", number, err)
	}
	return Comment{ID: comment.ID, Body: comment.Body}, nil
}

func (c *Client) ListPRComments(ctx context.Context, repo RepoRef, number int) ([]Comment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	comments, _, err := c.sdk.ListIssueComments(repo.Owner, repo.Name, int64(number), gitea.ListIssueCommentOptions{})
	if err != nil {
		return nil, fmt.Errorf("gitclient: list comments on PR #%d: %w", number, err)
	}
	out := make([]Comment, len(comments))
	for i, c := range comments {
		out[i] = Comment{ID: c.ID, Body: c.Body}
	}
	return out, nil
}

func (c *Client) GetBranchProtection(ctx context.Context, repo RepoRef, branch string) (BranchProtection, error) {
	if err := ctx.Err(); err != nil {
		return BranchProtection{}, err
	}
	bp, _, err := c.sdk.GetBranchProtection(repo.Owner, repo.Name, branch)
	if err != nil {
		return BranchProtection{}, fmt.Errorf("gitclient: get branch protection for %s: %w", branch, err)
	}
	return BranchProtection{
		BranchName:          bp.BranchName,
		RequiredApprovals:   int(bp.RequiredApprovals),
		RequireStatusChecks: bp.EnableStatusCheck,
	}, nil
}

var roleOrder = map[gitea.AccessMode]int{
	gitea.AccessModeNone:  0,
	gitea.AccessModeRead:  1,
	gitea.AccessModeWrite: 2,
	gitea.AccessModeAdmin: 3,
	gitea.AccessModeOwner: 4,
}

func toAccessMode(r Role) gitea.AccessMode {
	switch r {
	case RoleAdmin:
		return gitea.AccessModeAdmin
	case RoleWrite:
		return gitea.AccessModeWrite
	default:
		return gitea.AccessModeRead
	}
}

func (c *Client) VerifyCollaboratorRole(ctx context.Context, repo RepoRef, username string, minRole Role) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	perm, _, err := c.sdk.CollaboratorPermission(repo.Owner, repo.Name, username)
	if err != nil {
		return false, fmt.Errorf("gitclient: get collaborator permission for %s: %w", username, err)
	}
	return roleOrder[perm.Permission] >= roleOrder[toAccessMode(minRole)], nil
}

func (c *Client) CreateWebhook(ctx context.Context, repo RepoRef, cfg WebhookConfig) (Webhook, error) {
	if err := ctx.Err(); err != nil {
		return Webhook{}, err
	}
	config := map[string]string{"url": cfg.URL, "content_type": cfg.ContentType}
	if cfg.Secret != "" {
		config["secret"] = cfg.Secret
	}
	hook, _, err := c.sdk.CreateRepoHook(repo.Owner, repo.Name, gitea.CreateHookOption{
		Type:   gitea.HookTypeGitea,
		Config: config,
		Events: cfg.Events,
		Active: true,
	})
	if err != nil {
		return Webhook{}, fmt.Errorf("gitclient: create webhook on %s/%s: %w", repo.Owner, repo.Name, err)
	}
	return Webhook{ID: hook.ID, Active: hook.Active}, nil
}

// CreateOrReuseIssue searches repo's open issues for one whose title exactly
// matches title, returning it if found; otherwise it creates a new one.
// Reuse-by-title is what makes this safe to call again after a crash
// between creating an issue and the caller persisting its number.
func (c *Client) CreateOrReuseIssue(ctx context.Context, repo RepoRef, title, body string) (Issue, error) {
	if err := ctx.Err(); err != nil {
		return Issue{}, err
	}
	open, _, err := c.sdk.ListRepoIssues(repo.Owner, repo.Name, gitea.ListIssueOption{State: gitea.StateOpen})
	if err != nil {
		return Issue{}, fmt.Errorf("gitclient: list open issues on %s/%s: %w", repo.Owner, repo.Name, err)
	}
	for _, existing := range open {
		if existing.Title == title {
			return toIssue(existing), nil
		}
	}

	created, _, err := c.sdk.CreateIssue(repo.Owner, repo.Name, gitea.CreateIssueOption{Title: title, Body: body})
	if err != nil {
		return Issue{}, fmt.Errorf("gitclient: create issue on %s/%s: %w", repo.Owner, repo.Name, err)
	}
	return toIssue(created), nil
}

func (c *Client) ListIssueLabels(ctx context.Context, repo RepoRef, issueNumber int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	labels, _, err := c.sdk.GetIssueLabels(repo.Owner, repo.Name, int64(issueNumber), gitea.ListLabelsOptions{})
	if err != nil {
		return nil, fmt.Errorf("gitclient: get labels for issue #%d on %s/%s: %w", issueNumber, repo.Owner, repo.Name, err)
	}
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names, nil
}

func toIssue(iss *gitea.Issue) Issue {
	return Issue{Number: int(iss.Index), Title: iss.Title, Body: iss.Body}
}

func toPullRequest(pr *gitea.PullRequest) PullRequest {
	out := PullRequest{
		Number: int(pr.Index),
		Title:  pr.Title,
		Body:   pr.Body,
		State:  string(pr.State),
		URL:    pr.HTMLURL,
	}
	if pr.Head != nil {
		out.Head = pr.Head.Ref
	}
	if pr.Base != nil {
		out.Base = pr.Base.Ref
	}
	return out
}
