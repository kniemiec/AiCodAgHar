package fakes

import (
	"context"
	"fmt"
	"sync"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

// FakeGitHostClient is an in-memory GitHostClient. Tests preload state via
// the Set* helpers; nothing is fetched over the network.
type FakeGitHostClient struct {
	mu sync.Mutex

	files             map[string][]byte // "owner/name@ref/path"
	prs               map[string]gitclient.PullRequest
	nextPRNumber      map[string]int // "owner/name"
	comments          map[string][]gitclient.Comment
	branchProtections map[string]gitclient.BranchProtection
	collaboratorRoles map[string]gitclient.Role
	webhooks          map[string][]gitclient.Webhook
}

func repoKey(repo gitclient.RepoRef) string {
	return repo.Owner + "/" + repo.Name
}

// SetFileContents preloads a file so GetFileContents can return it.
func (f *FakeGitHostClient) SetFileContents(repo gitclient.RepoRef, ref, path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[fmt.Sprintf("%s@%s/%s", repoKey(repo), ref, path)] = content
}

func (f *FakeGitHostClient) GetFileContents(ctx context.Context, repo gitclient.RepoRef, path, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[fmt.Sprintf("%s@%s/%s", repoKey(repo), ref, path)]
	if !ok {
		return nil, fmt.Errorf("fake git host: no file %s@%s/%s", repoKey(repo), ref, path)
	}
	return content, nil
}

func (f *FakeGitHostClient) CreateOrUpdatePR(ctx context.Context, repo gitclient.RepoRef, branch, base, title, body string) (gitclient.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prs == nil {
		f.prs = map[string]gitclient.PullRequest{}
	}
	if f.nextPRNumber == nil {
		f.nextPRNumber = map[string]int{}
	}
	key := repoKey(repo)
	f.nextPRNumber[key]++
	pr := gitclient.PullRequest{
		Number: f.nextPRNumber[key],
		Title:  title,
		Body:   body,
		Head:   branch,
		Base:   base,
		State:  "open",
	}
	f.prs[fmt.Sprintf("%s#%d", key, pr.Number)] = pr
	return pr, nil
}

func (f *FakeGitHostClient) GetPR(ctx context.Context, repo gitclient.RepoRef, number int) (gitclient.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[fmt.Sprintf("%s#%d", repoKey(repo), number)]
	if !ok {
		return gitclient.PullRequest{}, fmt.Errorf("fake git host: no PR %s#%d", repoKey(repo), number)
	}
	return pr, nil
}

func (f *FakeGitHostClient) PostPRComment(ctx context.Context, repo gitclient.RepoRef, number int, body string) (gitclient.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[string][]gitclient.Comment{}
	}
	key := fmt.Sprintf("%s#%d", repoKey(repo), number)
	c := gitclient.Comment{ID: int64(len(f.comments[key]) + 1), Body: body}
	f.comments[key] = append(f.comments[key], c)
	return c, nil
}

func (f *FakeGitHostClient) ListPRComments(ctx context.Context, repo gitclient.RepoRef, number int) ([]gitclient.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fmt.Sprintf("%s#%d", repoKey(repo), number)
	out := make([]gitclient.Comment, len(f.comments[key]))
	copy(out, f.comments[key])
	return out, nil
}

// SetBranchProtection preloads a branch protection rule.
func (f *FakeGitHostClient) SetBranchProtection(repo gitclient.RepoRef, branch string, bp gitclient.BranchProtection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branchProtections == nil {
		f.branchProtections = map[string]gitclient.BranchProtection{}
	}
	f.branchProtections[repoKey(repo)+"/"+branch] = bp
}

func (f *FakeGitHostClient) GetBranchProtection(ctx context.Context, repo gitclient.RepoRef, branch string) (gitclient.BranchProtection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bp, ok := f.branchProtections[repoKey(repo)+"/"+branch]
	if !ok {
		return gitclient.BranchProtection{}, fmt.Errorf("fake git host: no branch protection for %s/%s", repoKey(repo), branch)
	}
	return bp, nil
}

// SetCollaboratorRole preloads a collaborator's role on a repo.
func (f *FakeGitHostClient) SetCollaboratorRole(repo gitclient.RepoRef, username string, role gitclient.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.collaboratorRoles == nil {
		f.collaboratorRoles = map[string]gitclient.Role{}
	}
	f.collaboratorRoles[repoKey(repo)+"/"+username] = role
}

var roleRank = map[gitclient.Role]int{
	gitclient.RoleRead:  1,
	gitclient.RoleWrite: 2,
	gitclient.RoleAdmin: 3,
}

func (f *FakeGitHostClient) VerifyCollaboratorRole(ctx context.Context, repo gitclient.RepoRef, username string, minRole gitclient.Role) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, ok := f.collaboratorRoles[repoKey(repo)+"/"+username]
	if !ok {
		return false, nil
	}
	return roleRank[role] >= roleRank[minRole], nil
}

func (f *FakeGitHostClient) CreateWebhook(ctx context.Context, repo gitclient.RepoRef, cfg gitclient.WebhookConfig) (gitclient.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.webhooks == nil {
		f.webhooks = map[string][]gitclient.Webhook{}
	}
	key := repoKey(repo)
	wh := gitclient.Webhook{ID: int64(len(f.webhooks[key]) + 1), Active: true}
	f.webhooks[key] = append(f.webhooks[key], wh)
	return wh, nil
}
