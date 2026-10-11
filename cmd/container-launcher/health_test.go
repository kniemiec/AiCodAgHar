package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func failing(t *testing.T, cfg config, ready bool) map[string]bool {
	t.Helper()
	var e atomic.Bool
	e.Store(ready)
	out := map[string]bool{}
	for _, c := range readiness(cfg, &e) {
		out[c.Name] = c.Fn(context.Background()) != nil
	}
	return out
}

func TestReadiness(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := failing(t, config{JailerBinary: bin, FirecrackerBinary: noexec, ImageRoot: dir}, false)
	if !got["egress"] {
		t.Error("egress must fail before setup completes")
	}
	if got["jailer_binary"] {
		t.Error("executable jailer should pass")
	}
	if !got["firecracker_binary"] {
		t.Error("non-executable firecracker should fail")
	}
	if got["image_root"] {
		t.Error("readable image root should pass")
	}

	got = failing(t, config{JailerBinary: bin, FirecrackerBinary: bin, ImageRoot: filepath.Join(dir, "missing")}, true)
	if got["egress"] || !got["image_root"] {
		t.Errorf("unexpected: %v", got)
	}
}
