// control-plane is the real, unprivileged production binary (see
// docs/plans/active/orchestrator-control-plane-mvp.md). Wires all the real
// client implementations — GitHostClient, PolicyClient, SecretsClient,
// ContainerLauncher, ApprovalVerifier, and (Phase 5) the Postgres
// AuditWriter — into internal/control-plane/coreloop and exposes a REST endpoint to
// create a task.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

func main() {
	logging.Setup("control-plane")

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	forgejoURL := requireEnv("FORGEJO_URL")
	forgejoToken := requireEnv("FORGEJO_TOKEN")
	pdpURL := requireEnv("PDP_MOCK_URL")
	secretsURL := requireEnv("SECRETS_MOCK_URL")
	launcherAddr := requireEnv("LAUNCHER_ADDR")
	orchestratorInstanceID := os.Getenv("ORCHESTRATOR_INSTANCE_ID")
	if orchestratorInstanceID == "" {
		orchestratorInstanceID = "control-plane-dev"
	}

	slog.Info("control-plane starting",
		"addr", addr, "forgejo_url", forgejoURL, "pdp_url", pdpURL, "secrets_url", secretsURL,
		"launcher_addr", launcherAddr, "orchestrator_instance_id", orchestratorInstanceID,
		"webhook_approval_enabled", os.Getenv("WEBHOOK_APPROVAL_ENABLED") == "true")

	git, err := gitclient.NewClient(forgejoURL, forgejoToken)
	if err != nil {
		log.Fatalf("control-plane: git client: %v", err)
	}

	policy := policyclient.NewClient(pdpURL)
	secrets := secretsclient.NewClient(secretsURL)

	launcher, err := launcherclient.NewClient(launcherAddr, launcherclient.TLSConfig{
		CACertPath:     requireEnv("TLS_CA_CERT"),
		ClientCertPath: requireEnv("TLS_CLIENT_CERT"),
		ClientKeyPath:  requireEnv("TLS_CLIENT_KEY"),
	})
	if err != nil {
		log.Fatalf("control-plane: launcher client: %v", err)
	}
	defer launcher.Close()

	auditWriter, err := audit.NewPostgresWriter(requireEnv("AUDIT_DB_DSN"))
	if err != nil {
		log.Fatalf("control-plane: audit writer: %v", err)
	}
	defer auditWriter.Close()

	taskStore, err := taskrunner.NewPostgresTaskStore(requireEnv("ORCHESTRATOR_DB_DSN"))
	if err != nil {
		log.Fatalf("control-plane: task store: %v", err)
	}
	defer taskStore.Close()

	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter, ForgejoURL: forgejoURL},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter, ForgejoURL: forgejoURL},
	)

	// Real ApprovalVerifier (design §4.4): the actual verification work
	// happens in handleForgejoWebhook; this just reads back what it
	// persisted. WEBHOOK_APPROVAL_ENABLED opts in explicitly — without a
	// webhook configured (e.g. local dev), NoopApprovalVerifier's fail-closed
	// default is safer than a verifier that can never actually be satisfied.
	var approvalVerifier approval.ApprovalVerifier = approval.NoopApprovalVerifier{}
	if os.Getenv("WEBHOOK_APPROVAL_ENABLED") == "true" {
		approvalVerifier = &approval.RealApprovalVerifier{Reader: taskrunner.NewTaskApprovalReader(taskStore)}
	}

	runner := taskrunner.NewRunner(taskrunner.Dependencies{
		Coreloop: coreloop.Dependencies{
			Policy:                 policy,
			Approval:               approvalVerifier,
			Audit:                  auditWriter,
			Executors:              registry,
			OrchestratorInstanceID: orchestratorInstanceID,
			IssueOpener:            git,
			PRCreator:              git,
		},
		Store:    taskStore,
		Launcher: launcher,
	})

	// Startup reconciliation (design §4.2): resume any task left
	// status=running from a prior process's in-flight execution — a
	// restart survives at any point, not only while waiting on approval.
	if err := runner.Reconcile(context.Background()); err != nil {
		log.Fatalf("control-plane: startup reconciliation: %v", err)
	}

	srv := &server{git: git, secrets: secrets, runner: runner}

	slog.Info("control-plane listening", "addr", addr)
	if err := http.ListenAndServe(addr, logRequests(newServeMux(srv))); err != nil {
		log.Fatalf("control-plane: %v", err)
	}
}

// logRequests logs each request with its response status and duration.
// GET /tasks/{id} logs at debug, since polling it would otherwise drown
// everything else at info.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/tasks/"):
			level = slog.LevelDebug
		}
		slog.Log(r.Context(), level, "http request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("control-plane: required env var %s is not set", name)
	}
	return v
}
