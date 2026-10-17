// control-plane is the real, unprivileged production binary (see
// docs/plans/active/orchestrator-control-plane-mvp.md). Wires all the real
// client implementations — GitHostClient, PolicyClient, SecretsClient,
// ContainerLauncher, ApprovalVerifier, and (Phase 5) the Postgres
// AuditWriter — into internal/control-plane/coreloop and exposes a REST endpoint to
// create a task.
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/buildinfo"
	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/coreloop"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/failure"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/instrumented"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
	"github.com/kniemiec/AiCodAgHar/internal/health"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

func main() {
	// Telemetry first, so the OTLP log sink (when enabled) sees startup too.
	tel, err := telemetry.Setup(context.Background(), "control-plane")
	if err != nil {
		logging.Setup("control-plane")
		log.Fatalf("control-plane: telemetry: %v", err)
	}
	logging.Setup("control-plane", tel.LogHandler)
	tel.RegisterBuildInfo()
	metrics := instrumented.NewMetrics(tel.Meter(), instrumented.Options{
		TargetRepos: envList("TELEMETRY_TARGET_REPOS"),
		StepIDs:     envList("TELEMETRY_STEP_IDS"),
	})

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
		"version", buildinfo.Version, "commit", buildinfo.Commit,
		"addr", addr, "forgejo_url", forgejoURL, "pdp_url", pdpURL, "secrets_url", secretsURL,
		"launcher_addr", launcherAddr, "orchestrator_instance_id", orchestratorInstanceID,
		"webhook_approval_enabled", os.Getenv("WEBHOOK_APPROVAL_ENABLED") == "true",
		"task_max_retries", envInt("TASK_MAX_RETRIES", taskrunner.DefaultMaxRetries),
		"heartbeat_interval", envDuration("HEARTBEAT_INTERVAL", 5*time.Minute),
		"stuck_task_interval", envDuration("STUCK_TASK_INTERVAL", defaultStuckInterval),
		"otlp_export", tel.LogHandler != nil)

	// Every dependency client is wrapped in its instrumented decorator
	// (MT-13); the raw clients are only used for Close/Ping below.
	gitRaw, err := gitclient.NewClient(forgejoURL, forgejoToken)
	if err != nil {
		log.Fatalf("control-plane: git client: %v", err)
	}
	git := instrumented.Git{Next: gitRaw, M: metrics}

	policy := instrumented.Policy{Next: policyclient.NewClient(pdpURL), M: metrics}
	secrets := instrumented.Secrets{Next: secretsclient.NewClient(secretsURL), M: metrics}

	launcherRaw, err := launcherclient.NewClient(launcherAddr, launcherclient.TLSConfig{
		CACertPath:     requireEnv("TLS_CA_CERT"),
		ClientCertPath: requireEnv("TLS_CLIENT_CERT"),
		ClientKeyPath:  requireEnv("TLS_CLIENT_KEY"),
	})
	if err != nil {
		log.Fatalf("control-plane: launcher client: %v", err)
	}
	defer launcherRaw.Close()
	launcher := instrumented.Launcher{Next: launcherRaw, M: metrics}

	auditRaw, err := audit.NewPostgresWriter(requireEnv("AUDIT_DB_DSN"))
	if err != nil {
		log.Fatalf("control-plane: audit writer: %v", err)
	}
	defer auditRaw.Close()
	auditWriter := instrumented.Audit{Next: auditRaw, M: metrics}

	pgStore, err := taskrunner.NewPostgresTaskStore(requireEnv("ORCHESTRATOR_DB_DSN"))
	if err != nil {
		log.Fatalf("control-plane: task store: %v", err)
	}
	defer pgStore.Close()
	// Task/round/phase/step metrics are derived from committed events
	// (design §6), never emitted at call sites.
	pgStore.SetEventObserver(metrics.Observe)
	taskStore := instrumented.Store{Next: pgStore, M: metrics}
	metrics.RegisterFleetGauges(taskStore, envDuration("STUCK_TASK_INTERVAL", defaultStuckInterval))

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
			HeartbeatInterval:      envDuration("HEARTBEAT_INTERVAL", 5*time.Minute),
		},
		Store:      taskStore,
		Launcher:   launcher,
		MaxRetries: envInt("TASK_MAX_RETRIES", taskrunner.DefaultMaxRetries),
		Metrics:    metrics,
	})

	// Startup reconciliation (design §4.2): resume any task left
	// status=running from a prior process's in-flight execution — a
	// restart survives at any point, not only while waiting on approval.
	if err := runner.Reconcile(context.Background()); err != nil {
		log.Fatalf("control-plane: startup reconciliation: %v", err)
	}

	srv := &server{git: git, secrets: secrets, runner: runner, metrics: metrics}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Readiness (design §8): both Postgres pools, plus the latest result of a
	// background prober for the two fail-closed dependencies.
	pdpProbe := health.NewProber("pdp", pdpURL)
	secretsProbe := health.NewProber("secrets_issuer", secretsURL)
	go pdpProbe.Run(ctx, probeInterval)
	go secretsProbe.Run(ctx, probeInterval)
	metrics.RegisterDependencyUp(map[string]func() bool{
		failure.DepPDP: pdpProbe.Up, failure.DepSecretsIssuer: secretsProbe.Up,
	})

	mux := newServeMux(srv)
	health.Register(mux,
		health.Check{Name: "orchestrator_db", Fn: pgStore.Ping},
		health.Check{Name: "audit_db", Fn: auditRaw.Ping},
		pdpProbe.Check(), secretsProbe.Check(),
	)

	// Admin listener: operator retry and the waiting/failed listing. Loopback
	// by default because there is no authentication yet (N4).
	adminAddr := os.Getenv("ADMIN_ADDR")
	if adminAddr == "" {
		adminAddr = "127.0.0.1:8090"
	}
	servers := []*http.Server{
		{Addr: addr, Handler: logRequests(mux)},
		{Addr: adminAddr, Handler: logRequests(newAdminMux(srv))},
	}
	errCh := make(chan error, len(servers))
	for _, hs := range servers {
		go func() {
			slog.Info("control-plane listening", "addr", hs.Addr)
			errCh <- hs.ListenAndServe()
		}()
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("control-plane: listener: %v", err)
		}
	case <-ctx.Done():
		slog.Info("control-plane shutting down")
	}

	// In-flight task goroutines are not awaited: restart reconciliation
	// resumes whatever they were doing (design §8).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, hs := range servers {
		if err := hs.Shutdown(shutdownCtx); err != nil {
			slog.Warn("control-plane: shutdown", "addr", hs.Addr, "error", err)
		}
	}
	if err := tel.Shutdown(shutdownCtx); err != nil {
		slog.Warn("control-plane: telemetry shutdown", "error", err)
	}
}

const (
	probeInterval        = 10 * time.Second
	shutdownTimeout      = 5 * time.Second
	defaultStuckInterval = 30 * time.Minute
)

// logRequests logs each request with its response status and duration.
// GET /tasks/{id} and the health endpoints log at debug, since polling them
// would otherwise drown everything else at info.
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
		case r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/tasks/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz"):
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

// envInt reads a positive integer env var, falling back to def when unset;
// a malformed value is a startup error rather than a silent default.
func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Fatalf("control-plane: %s must be a non-negative integer, got %q", name, v)
	}
	return n
}

// envDuration reads a Go duration env var ("5m"), falling back to def when unset.
func envDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		log.Fatalf("control-plane: %s must be a non-negative duration like 5m, got %q", name, v)
	}
	return d
}

// envList reads a comma-separated env var; unset or empty is nil.
func envList(name string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(name), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("control-plane: required env var %s is not set", name)
	}
	return v
}
