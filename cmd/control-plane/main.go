// control-plane is the real, unprivileged production binary (see
// docs/plans/active/orchestrator-control-plane-mvp.md). Wires all the real
// client implementations — GitHostClient, PolicyClient, SecretsClient,
// ContainerLauncher, ApprovalVerifier, and (Phase 5) the Postgres
// AuditWriter — into internal/coreloop and exposes a REST endpoint to
// create a task.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/kniemiec/AiCodAgHar/internal/approval"
	"github.com/kniemiec/AiCodAgHar/internal/audit"
	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/executor"
)

func main() {
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

	registry := executor.NewRegistry(
		&executor.AgentStageExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter, ForgejoURL: forgejoURL},
		&executor.DeterministicStepExecutor{Launcher: launcher, Secrets: secrets, Audit: auditWriter, ForgejoURL: forgejoURL},
	)

	srv := &server{
		git: git,
		loop: coreloop.Dependencies{
			Policy:                 policy,
			Approval:               approval.NoopApprovalVerifier{},
			Audit:                  auditWriter,
			Executors:              registry,
			OrchestratorInstanceID: orchestratorInstanceID,
		},
	}

	log.Printf("control-plane listening on %s", addr)
	if err := http.ListenAndServe(addr, logRequests(newServeMux(srv))); err != nil {
		log.Fatalf("control-plane: %v", err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("control-plane: required env var %s is not set", name)
	}
	return v
}
