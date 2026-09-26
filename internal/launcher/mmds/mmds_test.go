package mmds

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/launcher/fcapi"
)

// shortTempDir is t.TempDir(), but rooted at /tmp instead of $TMPDIR: on
// macOS, $TMPDIR nests under /var/folders/.../TestName<random>/001/, which
// comfortably exceeds AF_UNIX's 104-byte sockaddr_un.sun_path limit and
// fails bind(2) with EINVAL for any longer-named test. /tmp itself is
// always short, so hardcoding it here (test-only) keeps every socket path
// well under the limit on every platform.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mmds")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newFakeServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir := shortTempDir(t)
	socketPath := filepath.Join(dir, "fc.sock")
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(lis)
	t.Cleanup(func() {
		srv.Close()
		lis.Close()
	})
	return socketPath
}

func TestConfigure(t *testing.T) {
	var gotBody fcapi.MMDSConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/mmds/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	fc := fcapi.NewClient(sock)
	if err := Configure(context.Background(), fc, []string{"net1"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if gotBody.Version != "V2" {
		t.Errorf("body.Version = %q, want V2", gotBody.Version)
	}
	if len(gotBody.NetworkInterfaces) != 1 || gotBody.NetworkInterfaces[0] != "net1" {
		t.Errorf("body.NetworkInterfaces = %v, want [net1]", gotBody.NetworkInterfaces)
	}
}

func TestConfigure_NoNetworkInterfaces(t *testing.T) {
	var gotBody fcapi.MMDSConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/mmds/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	fc := fcapi.NewClient(sock)
	if err := Configure(context.Background(), fc, nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(gotBody.NetworkInterfaces) != 0 {
		t.Errorf("body.NetworkInterfaces = %v, want empty", gotBody.NetworkInterfaces)
	}
}

func TestInject(t *testing.T) {
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/mmds", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	fc := fcapi.NewClient(sock)
	data := map[string]string{
		"FORGEJO_TOKEN": "secret-token",
		"FORGEJO_URL":   "https://forgejo.example",
		"TARGET_REPO":   "org/repo",
		"BRANCH":        "add-feature",
	}
	if err := Inject(context.Background(), fc, data); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for k, v := range data {
		if gotBody[k] != v {
			t.Errorf("body[%q] = %q, want %q", k, gotBody[k], v)
		}
	}
}
