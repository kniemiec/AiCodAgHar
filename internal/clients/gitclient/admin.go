package gitclient

import (
	"context"
	"encoding/base64"
	"fmt"

	"code.gitea.io/sdk/gitea"
)

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// The methods in this file are deliberately NOT part of the GitHostClient
// interface: that interface is scoped to what the orchestrator's core loop
// needs at runtime (PR/comment/webhook operations). CreateRepo, CreateFile,
// and bot-account provisioning are one-time platform-bootstrapping
// operations, used only by cmd/seed (and, for CreateFile, by
// launcher-mock's commit-with-trailer fixture) — exposing them as extra
// exported methods on the concrete *Client keeps GitHostClient narrow while
// still reusing the same authenticated SDK wrapper.

// CreateRepo creates a repository owned by this Client's own authenticated
// user (the admin account in this plan's seed flow).
func (c *Client) CreateRepo(ctx context.Context, name string, autoInit bool) (RepoRef, error) {
	if err := ctx.Err(); err != nil {
		return RepoRef{}, err
	}
	repo, _, err := c.sdk.CreateRepo(gitea.CreateRepoOption{Name: name, AutoInit: autoInit})
	if err != nil {
		return RepoRef{}, fmt.Errorf("gitclient: create repo %s: %w", name, err)
	}
	return RepoRef{Owner: repo.Owner.UserName, Name: repo.Name}, nil
}

// DeleteRepo deletes a repository. Used by cmd/seed for idempotent
// re-seeding (delete-then-recreate) — errors are not distinguished from
// "did not exist," since seed only cares that the repo is gone afterward.
func (c *Client) DeleteRepo(ctx context.Context, repo RepoRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := c.sdk.DeleteRepo(repo.Owner, repo.Name)
	return err
}

// CreateFile commits a new file directly to branch (no PR), the same way
// cmd/seed lays down .harness/*.yaml and trivial source content, and the
// way launcher-mock's commit-with-trailer fixture commits its work.
func (c *Client) CreateFile(ctx context.Context, repo RepoRef, path, branch, message string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, err := c.sdk.CreateFile(repo.Owner, repo.Name, path, gitea.CreateFileOptions{
		FileOptions: gitea.FileOptions{Message: message, BranchName: branch},
		Content:     encodeBase64(content),
	})
	if err != nil {
		return fmt.Errorf("gitclient: create file %s on %s/%s@%s: %w", path, repo.Owner, repo.Name, branch, err)
	}
	return nil
}

// CreateFileOnNewBranch commits path as the first commit of a new branch
// cut from base — how cmd/seed creates the sample repo's feature branch.
func (c *Client) CreateFileOnNewBranch(ctx context.Context, repo RepoRef, path, base, newBranch, message string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, err := c.sdk.CreateFile(repo.Owner, repo.Name, path, gitea.CreateFileOptions{
		FileOptions: gitea.FileOptions{Message: message, BranchName: base, NewBranchName: newBranch},
		Content:     encodeBase64(content),
	})
	if err != nil {
		return fmt.Errorf("gitclient: create file %s on new branch %s (from %s) of %s/%s: %w", path, newBranch, base, repo.Owner, repo.Name, err)
	}
	return nil
}

// AddCollaborator grants username write access to repo — bot accounts need
// this to commit/comment on the sample repo cmd/seed creates.
func (c *Client) AddCollaborator(ctx context.Context, repo RepoRef, username string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	perm := gitea.AccessModeWrite
	_, err := c.sdk.AddCollaborator(repo.Owner, repo.Name, username, gitea.AddCollaboratorOption{Permission: &perm})
	if err != nil {
		return fmt.Errorf("gitclient: add collaborator %s to %s/%s: %w", username, repo.Owner, repo.Name, err)
	}
	return nil
}

// EnsureUser (re)creates username with exactly password — delete-then-
// recreate, the same idempotency strategy cmd/seed uses for repos, since
// the caller needs to know the password for later BasicAuth token minting
// and can't safely assume an already-existing user (e.g. one created
// manually, outside seed) has this password.
func (c *Client) EnsureUser(ctx context.Context, username, password, email string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _ = c.sdk.AdminDeleteUser(username) // best-effort; "did not exist" is fine
	mustChangePassword := false
	_, _, err := c.sdk.AdminCreateUser(gitea.CreateUserOption{
		Username:           username,
		Email:              email,
		Password:           password,
		MustChangePassword: &mustChangePassword,
	})
	if err != nil {
		return fmt.Errorf("gitclient: create user %s: %w", username, err)
	}
	return nil
}

// MintAccessToken authenticates as username/password (not this Client's own
// credentials) to mint that user's own token — Forgejo's CreateAccessToken
// API requires HTTP Basic auth as the token's owner, not an admin token +
// sudo. tokenName must be unique per user; pass a fresh value on each call
// (e.g. including a timestamp) to keep re-seeding idempotent.
func MintAccessToken(ctx context.Context, baseURL, username, password, tokenName string, scopes ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	userClient, err := gitea.NewClient(baseURL, gitea.SetBasicAuth(username, password), gitea.SetContext(ctx))
	if err != nil {
		return "", fmt.Errorf("gitclient: basic-auth client for %s: %w", username, err)
	}
	tokenScopes := make([]gitea.AccessTokenScope, len(scopes))
	for i, s := range scopes {
		tokenScopes[i] = gitea.AccessTokenScope(s)
	}
	token, _, err := userClient.CreateAccessToken(gitea.CreateAccessTokenOption{Name: tokenName, Scopes: tokenScopes})
	if err != nil {
		return "", fmt.Errorf("gitclient: mint access token for %s: %w", username, err)
	}
	return token.Token, nil
}
