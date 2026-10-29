package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveTimeout_NoCallerValueUsesDefault(t *testing.T) {
	got := resolveTimeout(0, 1800)
	if got != 1800*time.Second {
		t.Errorf("resolveTimeout(0, 1800) = %s, want 1800s", got)
	}
}

func TestResolveTimeout_NonPositiveCallerValueUsesDefault(t *testing.T) {
	got := resolveTimeout(-5, 1800)
	if got != 1800*time.Second {
		t.Errorf("resolveTimeout(-5, 1800) = %s, want 1800s", got)
	}
}

func TestResolveTimeout_PositiveCallerValueOverridesDefault(t *testing.T) {
	got := resolveTimeout(60, 1800)
	if got != 60*time.Second {
		t.Errorf("resolveTimeout(60, 1800) = %s, want 60s", got)
	}
}

func TestStatusForCancellation_Killed(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errInvocationKilled)
	if got := statusForCancellation(ctx); got != "killed" {
		t.Errorf("statusForCancellation = %q, want killed", got)
	}
}

func TestStatusForCancellation_OtherCauseIsError(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("some other cancellation"))
	if got := statusForCancellation(ctx); got != "error" {
		t.Errorf("statusForCancellation = %q, want error", got)
	}
}

func TestStatusForCancellation_PlainCancelIsError(t *testing.T) {
	// A context canceled without a cause (context.WithCancel, or the
	// caller's own request context) must never read as "killed" — only an
	// explicit Kill sets errInvocationKilled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := statusForCancellation(ctx); got != "error" {
		t.Errorf("statusForCancellation = %q, want error", got)
	}
}

func TestTeardown_RemovesJailDirectoryTree(t *testing.T) {
	dir := t.TempDir()
	jailDir := filepath.Join(dir, "jail")
	// Mirrors what a real invocation leaves under jailDir: the api socket
	// and staged images all live under root/, covered by one RemoveAll.
	if err := os.MkdirAll(filepath.Join(jailDir, "root", "run"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jailDir, "root", "run", "firecracker.socket"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jailDir, "root", "rootfs.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	teardown(context.Background(), jailDir)

	if _, err := os.Stat(jailDir); !os.IsNotExist(err) {
		t.Errorf("jailDir still exists after teardown (err=%v), want removed", err)
	}
}

func TestTeardown_NonexistentDirIsNotAnError(t *testing.T) {
	// teardown is unconditionally deferred, including on paths where the
	// jail directory was never created (e.g. jailer.Config.Validate failed
	// before anything was staged) — must not panic or block.
	teardown(context.Background(), filepath.Join(t.TempDir(), "never-created"))
}

func TestAddProxyEnv_OverridesCallerValues(t *testing.T) {
	env := mergeAgentEnv(map[string]string{"HTTP_PROXY": "http://caller:1"}, "task", "", "")
	addProxyEnv(env, "http://172.16.0.1:8888")
	if env["HTTP_PROXY"] != "http://172.16.0.1:8888" || env["HTTPS_PROXY"] != "http://172.16.0.1:8888" {
		t.Errorf("proxy env = %q / %q, want both set to the launcher's proxy", env["HTTP_PROXY"], env["HTTPS_PROXY"])
	}
}
