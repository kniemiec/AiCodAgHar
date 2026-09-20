package main

import (
	"log"
	"os"
	"strconv"
)

// config holds cmd/container-launcher's env-var contract
// (docs/designs/completed/container-launcher-design.md §17).
type config struct {
	Addr       string
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

	// DefaultTimeoutSeconds is the hard ceiling applied whenever a caller
	// sends LaunchRequest.TimeoutSeconds <= 0 (design §8) — true of both
	// executors today. A positive caller-supplied value overrides it
	// (resolveTimeout, launch.go).
	DefaultTimeoutSeconds int
}

func loadConfig() config {
	cfg := config{
		Addr:                  os.Getenv("ADDR"),
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
		DefaultTimeoutSeconds: envInt("LAUNCHER_DEFAULT_TIMEOUT_SECONDS", 1800),
	}
	if cfg.Addr == "" {
		cfg.Addr = ":9443"
	}
	return cfg
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
