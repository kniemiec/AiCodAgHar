// Package health serves the liveness and readiness endpoints both binaries
// expose (HL-1). Liveness means "the process is serving"; readiness means
// every registered Check currently passes. Failure details are logged, never
// returned, because the endpoints are reachable by anyone who can reach the
// listener.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Check is one named readiness condition.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// checkTimeout bounds each readiness evaluation so a hung dependency makes
// /readyz fail rather than hang.
const checkTimeout = 2 * time.Second

// Register mounts GET /healthz and GET /readyz on mux.
func Register(mux *http.ServeMux, checks ...Check) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		defer cancel()
		var failing []string
		for _, c := range checks {
			if err := c.Fn(ctx); err != nil {
				slog.WarnContext(r.Context(), "readiness check failed", "check", c.Name, "error", err)
				failing = append(failing, c.Name)
			}
		}
		if len(failing) > 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "failing": failing})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var errNotProbed = errors.New("health: not probed yet")

// Prober periodically checks that a dependency's base URL answers at all.
// The PDP and secrets-issuer contracts define no health endpoint, so any
// HTTP response counts as reachable and only a transport error (connect
// refused, timeout, DNS) counts as down.
type Prober struct {
	name, url string
	client    *http.Client

	mu  sync.RWMutex
	err error
}

// NewProber returns a Prober for url. Call Run to keep it fresh.
func NewProber(name, url string) *Prober {
	return &Prober{name: name, url: url, client: &http.Client{Timeout: checkTimeout}, err: errNotProbed}
}

// Probe performs one probe and records the result.
func (p *Prober) Probe(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err == nil {
		var resp *http.Response
		if resp, err = p.client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

// Run probes immediately and then every interval until ctx is done.
func (p *Prober) Run(ctx context.Context, interval time.Duration) {
	p.Probe(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Probe(ctx)
		}
	}
}

// Up reports whether the latest probe reached the dependency (false before
// the first probe) — the harness_dependency_up gauge's source.
func (p *Prober) Up() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.err == nil
}

// Check adapts the latest probe result into a readiness Check. Before the
// first probe completes it reports not ready.
func (p *Prober) Check() Check {
	return Check{Name: p.name, Fn: func(context.Context) error {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.err
	}}
}
