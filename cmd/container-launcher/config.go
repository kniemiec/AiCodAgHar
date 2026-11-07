package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
)

// config holds cmd/container-launcher's env-var contract
// (docs/designs/completed/container-launcher-design.md §17).
type config struct {
	Addr string
	// HealthAddr is the plain-HTTP /healthz,/readyz listener (LAUNCHER_HEALTH_ADDR).
	HealthAddr string
	CACert     string
	ServerCert string
	ServerKey  string

	ChrootBaseDir     string
	ImageRoot         string
	JailerBinary      string
	FirecrackerBinary string
	JailerUID         int
	JailerGID         int

	// LLMAPIKey/LLMModel are the coding agent's own LLM credentials —
	// platform-level, not per-invocation (unlike FORGEJO_TOKEN, which
	// arrives per-invocation in LaunchRequest.Env). Optional: a deployment
	// that only ever runs deterministic-step invocations never renders a
	// prompt, so never needs these. Merged into the MMDS payload only when
	// a prompt was actually rendered (cmd/container-launcher/launch.go).
	LLMAPIKey string
	LLMModel  string
	// LLMBaseURL optionally points the agent at a non-default LLM endpoint
	// (e.g. a local Ollama's OpenAI-compatible http://host:11434/v1).
	// Empty leaves the provider's default.
	LLMBaseURL string

	// DefaultTimeoutSeconds is the hard ceiling applied whenever a caller
	// sends LaunchRequest.TimeoutSeconds <= 0 (design §8) — true of both
	// executors today. A positive caller-supplied value overrides it
	// (resolveTimeout, launch.go).
	DefaultTimeoutSeconds int

	// FailedLogDir, when set (LAUNCHER_FAILED_LOG_DIR), is where a failed
	// invocation's full (redacted, ANSI-stripped) guest event log is
	// written as <invocation_id>.log. Empty disables it: the log is
	// sensitive (agent output), so it stays off disk unless asked for.
	FailedLogDir string
}

func loadConfig() config {
	cfg := config{
		Addr:                  os.Getenv("ADDR"),
		HealthAddr:            os.Getenv("LAUNCHER_HEALTH_ADDR"),
		CACert:                requireEnv("TLS_CA_CERT"),
		ServerCert:            requireEnv("TLS_SERVER_CERT"),
		ServerKey:             requireEnv("TLS_SERVER_KEY"),
		ChrootBaseDir:         requireEnv("LAUNCHER_CHROOT_BASE_DIR"),
		ImageRoot:             requireEnv("LAUNCHER_IMAGE_ROOT"),
		JailerBinary:          requireEnv("LAUNCHER_JAILER_BINARY"),
		FirecrackerBinary:     requireEnv("LAUNCHER_FIRECRACKER_BINARY"),
		JailerUID:             envInt("LAUNCHER_JAILER_UID", 123150),
		JailerGID:             envInt("LAUNCHER_JAILER_GID", 123150),
		LLMAPIKey:             os.Getenv("LAUNCHER_LLM_API_KEY"),
		LLMModel:              os.Getenv("LAUNCHER_LLM_MODEL"),
		LLMBaseURL:            os.Getenv("LAUNCHER_LLM_BASE_URL"),
		FailedLogDir:          os.Getenv("LAUNCHER_FAILED_LOG_DIR"),
		DefaultTimeoutSeconds: envInt("LAUNCHER_DEFAULT_TIMEOUT_SECONDS", 1800),
	}
	if err := validateLLMBaseURL(cfg.LLMBaseURL); err != nil {
		log.Fatalf("container-launcher: LAUNCHER_LLM_BASE_URL: %v", err)
	}
	if cfg.Addr == "" {
		cfg.Addr = ":9443"
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = "127.0.0.1:9444"
	}
	return cfg
}

// validateLLMBaseURL rejects values OpenHands/LiteLLM would otherwise
// misparse silently inside the guest (e.g. "192.168.1.14:11434/v1" with no
// scheme). Empty is valid: it leaves the provider's default endpoint.
func validateLLMBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q must be an absolute http(s) URL such as http://host:11434/v1", raw)
	}
	return nil
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("container-launcher: required env var %s is not set", name)
	}
	return v
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("container-launcher: env var %s must be an integer, got %q", name, v)
	}
	return n
}
