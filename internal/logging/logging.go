// Package logging configures the process-wide log/slog default logger
// shared by control-plane and container-launcher.
package logging

import (
	"log/slog"
	"os"
	"slices"
	"strings"
)

// Setup installs a slog default logger tagged with component, configured
// from LOG_LEVEL (debug|info|warn|error, default info) and LOG_FORMAT
// (text|json, default text). slog.SetDefault also redirects the stdlib log
// package, so remaining log.Printf/log.Fatalf calls go through the same
// handler (at info level).
func Setup(component string) {
	opts := &slog.HandlerOptions{Level: parseLevel(os.Getenv("LOG_LEVEL"))}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h).With("component", component))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Truncate shortens s to at most n bytes for debug output, marking the cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// Keys returns m's keys, sorted — for logging an env map without its values,
// which carry credentials.
func Keys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
