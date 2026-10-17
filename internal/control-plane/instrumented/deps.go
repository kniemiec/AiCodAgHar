package instrumented

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/status"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

// The decorators below time every dependency call and record
// harness_dependency_requests_total / _request_duration_seconds (MT-13).
// They don't classify errors: the call sites already wrap each error with
// its failure class and dependency (design §2, ER-4).

func attrString(k, v string) attribute.KeyValue { return attribute.String(k, v) }

// record notes one finished call. code is the gRPC code for the launcher
// and empty elsewhere.
func (m *Metrics) record(ctx context.Context, dep, method string, start time.Time, err error, code string) {
	if m == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	m.depRequests.Add(ctx, 1, telemetry.Attrs("dependency", dep, "method", method, "outcome", outcome, "code", code))
	m.depDuration.Record(ctx, time.Since(start).Seconds(), telemetry.Attrs("dependency", dep, "method", method, "outcome", outcome))
}

// Policy decorates a PolicyClient.
type Policy struct {
	Next policyclient.PolicyClient
	M    *Metrics
}

func (p Policy) Evaluate(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
	start := time.Now()
	d, err := p.Next.Evaluate(ctx, req)
	p.M.record(ctx, failure.DepPDP, "evaluate", start, err, "")
	return d, err
}

// Secrets decorates a SecretsClient.
type Secrets struct {
	Next secretsclient.SecretsClient
	M    *Metrics
}

func (s Secrets) IssueCredential(ctx context.Context, req secretsclient.CredentialRequest) (secretsclient.Credential, error) {
	start := time.Now()
	c, err := s.Next.IssueCredential(ctx, req)
	s.M.record(ctx, failure.DepSecretsIssuer, "issue_credential", start, err, "")
	return c, err
}

func (s Secrets) RevokeCredential(ctx context.Context, credentialID string) error {
	start := time.Now()
	err := s.Next.RevokeCredential(ctx, credentialID)
	s.M.record(ctx, failure.DepSecretsIssuer, "revoke_credential", start, err, "")
	return err
}

func (s Secrets) GetWebhookSecret(ctx context.Context, targetRepo string) (string, error) {
	start := time.Now()
	v, err := s.Next.GetWebhookSecret(ctx, targetRepo)
	s.M.record(ctx, failure.DepSecretsIssuer, "get_webhook_secret", start, err, "")
	return v, err
}

// Launcher decorates a ContainerLauncher; its code label is the gRPC code.
type Launcher struct {
	Next launcherclient.ContainerLauncher
	M    *Metrics
}

func (l Launcher) Launch(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
	start := time.Now()
	r, err := l.Next.Launch(ctx, req)
	l.M.record(ctx, failure.DepLauncher, "launch", start, err, status.Code(err).String())
	return r, err
}

func (l Launcher) Kill(ctx context.Context, invocationID string) error {
	start := time.Now()
	err := l.Next.Kill(ctx, invocationID)
	l.M.record(ctx, failure.DepLauncher, "kill", start, err, status.Code(err).String())
	return err
}

func (l Launcher) ExtractLogs(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
	start := time.Now()
	b, err := l.Next.ExtractLogs(ctx, invocationID)
	l.M.record(ctx, failure.DepLauncher, "extract_logs", start, err, status.Code(err).String())
	return b, err
}

// Audit decorates an AuditWriter; the audit trail lives in Postgres.
type Audit struct {
	Next audit.AuditWriter
	M    *Metrics
}

func (a Audit) WriteInvocationRecord(ctx context.Context, rec audit.InvocationAuditRecord) error {
	start := time.Now()
	err := a.Next.WriteInvocationRecord(ctx, rec)
	a.M.record(ctx, failure.DepPostgres, "audit.write", start, err, "")
	return err
}

func (a Audit) GetTaskChain(ctx context.Context, taskID string) ([]audit.InvocationAuditRecord, error) {
	start := time.Now()
	recs, err := a.Next.GetTaskChain(ctx, taskID)
	a.M.record(ctx, failure.DepPostgres, "audit.get_chain", start, err, "")
	return recs, err
}

// Git decorates a GitHostClient.
type Git struct {
	Next gitclient.GitHostClient
	M    *Metrics
}

func (g Git) done(ctx context.Context, method string, start time.Time, err error) {
	g.M.record(ctx, failure.DepGitHost, method, start, err, "")
}

func (g Git) GetFileContents(ctx context.Context, repo gitclient.RepoRef, path, ref string) ([]byte, error) {
	start := time.Now()
	b, err := g.Next.GetFileContents(ctx, repo, path, ref)
	g.done(ctx, "get_file_contents", start, err)
	return b, err
}

func (g Git) CreateOrReuseBranch(ctx context.Context, repo gitclient.RepoRef, branch, base string) (gitclient.Branch, error) {
	start := time.Now()
	b, err := g.Next.CreateOrReuseBranch(ctx, repo, branch, base)
	g.done(ctx, "create_or_reuse_branch", start, err)
	return b, err
}

func (g Git) CreateOrUpdatePR(ctx context.Context, repo gitclient.RepoRef, branch, base, title, body string) (gitclient.PullRequest, error) {
	start := time.Now()
	pr, err := g.Next.CreateOrUpdatePR(ctx, repo, branch, base, title, body)
	g.done(ctx, "create_or_update_pr", start, err)
	return pr, err
}

func (g Git) GetPR(ctx context.Context, repo gitclient.RepoRef, number int) (gitclient.PullRequest, error) {
	start := time.Now()
	pr, err := g.Next.GetPR(ctx, repo, number)
	g.done(ctx, "get_pr", start, err)
	return pr, err
}

func (g Git) PostPRComment(ctx context.Context, repo gitclient.RepoRef, number int, body string) (gitclient.Comment, error) {
	start := time.Now()
	c, err := g.Next.PostPRComment(ctx, repo, number, body)
	g.done(ctx, "post_pr_comment", start, err)
	return c, err
}

func (g Git) ListPRComments(ctx context.Context, repo gitclient.RepoRef, number int) ([]gitclient.Comment, error) {
	start := time.Now()
	cs, err := g.Next.ListPRComments(ctx, repo, number)
	g.done(ctx, "list_pr_comments", start, err)
	return cs, err
}

func (g Git) GetBranchProtection(ctx context.Context, repo gitclient.RepoRef, branch string) (gitclient.BranchProtection, error) {
	start := time.Now()
	bp, err := g.Next.GetBranchProtection(ctx, repo, branch)
	g.done(ctx, "get_branch_protection", start, err)
	return bp, err
}

func (g Git) VerifyCollaboratorRole(ctx context.Context, repo gitclient.RepoRef, username string, minRole gitclient.Role) (bool, error) {
	start := time.Now()
	ok, err := g.Next.VerifyCollaboratorRole(ctx, repo, username, minRole)
	g.done(ctx, "verify_collaborator_role", start, err)
	return ok, err
}

func (g Git) CreateWebhook(ctx context.Context, repo gitclient.RepoRef, cfg gitclient.WebhookConfig) (gitclient.Webhook, error) {
	start := time.Now()
	w, err := g.Next.CreateWebhook(ctx, repo, cfg)
	g.done(ctx, "create_webhook", start, err)
	return w, err
}

func (g Git) CreateOrReuseIssue(ctx context.Context, repo gitclient.RepoRef, title, body string) (gitclient.Issue, error) {
	start := time.Now()
	i, err := g.Next.CreateOrReuseIssue(ctx, repo, title, body)
	g.done(ctx, "create_or_reuse_issue", start, err)
	return i, err
}

func (g Git) ListIssueLabels(ctx context.Context, repo gitclient.RepoRef, issueNumber int) ([]string, error) {
	start := time.Now()
	ls, err := g.Next.ListIssueLabels(ctx, repo, issueNumber)
	g.done(ctx, "list_issue_labels", start, err)
	return ls, err
}

var (
	_ policyclient.PolicyClient        = Policy{}
	_ secretsclient.SecretsClient      = Secrets{}
	_ launcherclient.ContainerLauncher = Launcher{}
	_ audit.AuditWriter                = Audit{}
	_ gitclient.GitHostClient          = Git{}
)
