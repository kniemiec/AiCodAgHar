package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestHealthzAlwaysOK(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Check{Name: "db", Fn: func(context.Context) error { return errors.New("down") }})
	if code, _ := get(t, mux, "/healthz"); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
}

func TestReadyzReportsFailingChecksWithoutDetail(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux,
		Check{Name: "ok", Fn: func(context.Context) error { return nil }},
		Check{Name: "orchestrator_db", Fn: func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: refused") }},
	)
	code, body := get(t, mux, "/readyz")
	if code != 503 || !strings.Contains(body, "orchestrator_db") {
		t.Fatalf("got %d %s", code, body)
	}
	if strings.Contains(body, "10.0.0.5") || strings.Contains(body, `"ok"`) {
		t.Fatalf("body leaks detail or lists passing check: %s", body)
	}
}

func TestReadyzOKWhenAllPass(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Check{Name: "a", Fn: func(context.Context) error { return nil }})
	if code, _ := get(t, mux, "/readyz"); code != 200 {
		t.Fatalf("readyz = %d", code)
	}
}

func TestProberAnyHTTPResponseIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	p := NewProber("pdp", srv.URL)
	if p.Check().Fn(context.Background()) == nil {
		t.Fatal("must not be ready before first probe")
	}
	p.Probe(context.Background())
	if err := p.Check().Fn(context.Background()); err != nil {
		t.Fatalf("404 should count as reachable: %v", err)
	}
	srv.Close()
	p.Probe(context.Background())
	if p.Check().Fn(context.Background()) == nil {
		t.Fatal("closed server should be unreachable")
	}
}

func TestProberRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewProber("x", "http://127.0.0.1:1").Run(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
