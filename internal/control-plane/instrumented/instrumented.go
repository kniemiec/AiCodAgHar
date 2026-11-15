// Package instrumented decorates the control-plane's dependency clients
// with request metrics (MT-13): every call is timed and counted in
// harness_dependency_requests_total / _duration_seconds with its dependency
// and method. The decorators change nothing else — errors pass through
// unwrapped, because failure classes are assigned at the call sites
// (coreloop, executor, taskrunner) where the §2 table's context is known.
package instrumented

import (
	"context"
	"time"

	"google.golang.org/grpc/status"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// record times one call that started at start.
func record(ctx context.Context, m *telemetry.ControlPlane, dep, method string, start time.Time, err error) {
	m.DependencyCall(ctx, dep, method, "", time.Since(start), err)
}

// Policy decorates a PolicyClient.
type Policy struct {
	Next    policyclient.PolicyClient
	Metrics *telemetry.ControlPlane
}

func (p Policy) Evaluate(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
	start := time.Now()
	d, err := p.Next.Evaluate(ctx, req)
	record(ctx, p.Metrics, failure.DepPDP, "Evaluate", start, err)
	return d, err
}

// Secrets decorates a SecretsClient.
type Secrets struct {
	Next    secretsclient.SecretsClient
	Metrics *telemetry.ControlPlane
}

func (s Secrets) IssueCredential(ctx context.Context, req secretsclient.CredentialRequest) (secretsclient.Credential, error) {
	start := time.Now()
	c, err := s.Next.IssueCredential(ctx, req)
	record(ctx, s.Metrics, failure.DepSecretsIssuer, "IssueCredential", start, err)
	return c, err
}

func (s Secrets) RevokeCredential(ctx context.Context, credentialID string) error {
	start := time.Now()
	err := s.Next.RevokeCredential(ctx, credentialID)
	record(ctx, s.Metrics, failure.DepSecretsIssuer, "RevokeCredential", start, err)
	return err
}

func (s Secrets) GetWebhookSecret(ctx context.Context, targetRepo string) (string, error) {
	start := time.Now()
	v, err := s.Next.GetWebhookSecret(ctx, targetRepo)
	record(ctx, s.Metrics, failure.DepSecretsIssuer, "GetWebhookSecret", start, err)
	return v, err
}

// Launcher decorates a ContainerLauncher, recording the gRPC code as well.
type Launcher struct {
	Next    launcherclient.ContainerLauncher
	Metrics *telemetry.ControlPlane
}

func (l Launcher) record(ctx context.Context, method string, start time.Time, err error) {
	l.Metrics.DependencyCall(ctx, failure.DepLauncher, method, status.Code(err).String(), time.Since(start), err)
}

func (l Launcher) Launch(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
	start := time.Now()
	r, err := l.Next.Launch(ctx, req)
	l.record(ctx, "Launch", start, err)
	return r, err
}

func (l Launcher) Kill(ctx context.Context, invocationID string) error {
	start := time.Now()
	err := l.Next.Kill(ctx, invocationID)
	l.record(ctx, "Kill", start, err)
	return err
}

func (l Launcher) ExtractLogs(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
	start := time.Now()
	b, err := l.Next.ExtractLogs(ctx, invocationID)
	l.record(ctx, "ExtractLogs", start, err)
	return b, err
}

// Audit decorates an AuditWriter (dependency "postgres").
type Audit struct {
	Next    audit.AuditWriter
	Metrics *telemetry.ControlPlane
}

func (a Audit) WriteInvocationRecord(ctx context.Context, rec audit.InvocationAuditRecord) error {
	start := time.Now()
	err := a.Next.WriteInvocationRecord(ctx, rec)
	record(ctx, a.Metrics, failure.DepPostgres, "audit.WriteInvocationRecord", start, err)
	return err
}

func (a Audit) GetTaskChain(ctx context.Context, taskID string) ([]audit.InvocationAuditRecord, error) {
	start := time.Now()
	recs, err := a.Next.GetTaskChain(ctx, taskID)
	record(ctx, a.Metrics, failure.DepPostgres, "audit.GetTaskChain", start, err)
	return recs, err
}

// Git decorates a GitHostClient (dependency "git_host").
type Git struct {
	Next    gitclient.GitHostClient
	Metrics *telemetry.ControlPlane
}

func (g Git) rec(ctx context.Context, method string, start time.Time, err error) {
	record(ctx, g.Metrics, failure.DepGitHost, method, start, err)
}

func (g Git) GetFileContents(ctx context.Context, repo gitclient.RepoRef, path, ref string) ([]byte, error) {
	start := time.Now()
	b, err := g.Next.GetFileContents(ctx, repo, path, ref)
	g.rec(ctx, "GetFileContents", start, err)
	return b, err
}

func (g Git) CreateOrReuseBranch(ctx context.Context, repo gitclient.RepoRef, branch, base string) (gitclient.Branch, error) {
	start := time.Now()
	b, err := g.Next.CreateOrReuseBranch(ctx, repo, branch, base)
	g.rec(ctx, "CreateOrReuseBranch", start, err)
	return b, err
}

func (g Git) CreateOrUpdatePR(ctx context.Context, repo gitclient.RepoRef, branch, base, title, body string) (gitclient.PullRequest, error) {
	start := time.Now()
	pr, err := g.Next.CreateOrUpdatePR(ctx, repo, branch, base, title, body)
	g.rec(ctx, "CreateOrUpdatePR", start, err)
	return pr, err
}

func (g Git) GetPR(ctx context.Context, repo gitclient.RepoRef, number int) (gitclient.PullRequest, error) {
	start := time.Now()
	pr, err := g.Next.GetPR(ctx, repo, number)
	g.rec(ctx, "GetPR", start, err)
	return pr, err
}

func (g Git) PostPRComment(ctx context.Context, repo gitclient.RepoRef, number int, body string) (gitclient.Comment, error) {
	start := time.Now()
	c, err := g.Next.PostPRComment(ctx, repo, number, body)
	g.rec(ctx, "PostPRComment", start, err)
	return c, err
}

func (g Git) ListPRComments(ctx context.Context, repo gitclient.RepoRef, number int) ([]gitclient.Comment, error) {
	start := time.Now()
	cs, err := g.Next.ListPRComments(ctx, repo, number)
	g.rec(ctx, "ListPRComments", start, err)
	return cs, err
}

func (g Git) GetBranchProtection(ctx context.Context, repo gitclient.RepoRef, branch string) (gitclient.BranchProtection, error) {
	start := time.Now()
	bp, err := g.Next.GetBranchProtection(ctx, repo, branch)
	g.rec(ctx, "GetBranchProtection", start, err)
	return bp, err
}

func (g Git) VerifyCollaboratorRole(ctx context.Context, repo gitclient.RepoRef, username string, minRole gitclient.Role) (bool, error) {
	start := time.Now()
	ok, err := g.Next.VerifyCollaboratorRole(ctx, repo, username, minRole)
	g.rec(ctx, "VerifyCollaboratorRole", start, err)
	return ok, err
}

func (g Git) CreateWebhook(ctx context.Context, repo gitclient.RepoRef, cfg gitclient.WebhookConfig) (gitclient.Webhook, error) {
	start := time.Now()
	w, err := g.Next.CreateWebhook(ctx, repo, cfg)
	g.rec(ctx, "CreateWebhook", start, err)
	return w, err
}

func (g Git) CreateOrReuseIssue(ctx context.Context, repo gitclient.RepoRef, title, body string) (gitclient.Issue, error) {
	start := time.Now()
	i, err := g.Next.CreateOrReuseIssue(ctx, repo, title, body)
	g.rec(ctx, "CreateOrReuseIssue", start, err)
	return i, err
}

func (g Git) ListIssueLabels(ctx context.Context, repo gitclient.RepoRef, issueNumber int) ([]string, error) {
	start := time.Now()
	ls, err := g.Next.ListIssueLabels(ctx, repo, issueNumber)
	g.rec(ctx, "ListIssueLabels", start, err)
	return ls, err
}

var (
	_ policyclient.PolicyClient        = Policy{}
	_ secretsclient.SecretsClient      = Secrets{}
	_ launcherclient.ContainerLauncher = Launcher{}
	_ audit.AuditWriter                = Audit{}
	_ gitclient.GitHostClient          = Git{}
)
