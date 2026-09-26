package vsock

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortTempDir is t.TempDir(), but rooted at /tmp instead of $TMPDIR: on
// macOS, $TMPDIR nests under /var/folders/.../TestName<random>/001/, which
// combined with "<name>_53" comfortably exceeds AF_UNIX's 104-byte
// sockaddr_un.sun_path limit and fails bind(2) with EINVAL for any
// longer-named test. /tmp itself is always short, so hardcoding it here
// (test-only; production paths come from the jailer's own short chroot
// dir) keeps every socket path well under the limit on every platform.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vsock")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// dialGuest is called from a background goroutine, so it must not call any
// testing.T method that can invoke FailNow (Fatal/Fatalf) — those are only
// safe from the goroutine running the test itself. Logf is safe.
func dialGuest(t *testing.T, path string) net.Conn {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		conn, err = net.Dial("unix", path)
		if err == nil {
			return conn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("dial %s: %v", path, err)
	return nil
}

func TestListen_Path(t *testing.T) {
	dir := shortTempDir(t)
	udsPath := filepath.Join(dir, "vsock.sock")

	l, err := Listen(udsPath, 53, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	if l.path != udsPath+"_53" {
		t.Errorf("path = %q, want %q", l.path, udsPath+"_53")
	}
}

func TestWaitForResult_Success(t *testing.T) {
	dir := shortTempDir(t)
	udsPath := filepath.Join(dir, "vsock.sock")

	l, err := Listen(udsPath, 53, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn := dialGuest(t, udsPath+"_53")
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(`{"type":"event","line":"starting openhands"}` + "\n"))
		_, _ = conn.Write([]byte(`{"type":"event","line":"cloning repo"}` + "\n"))
		_, _ = conn.Write([]byte(`{"type":"structured_result","data":{"has_blocking_finding":false}}` + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := l.WaitForResult(ctx)
	if err != nil {
		t.Fatalf("WaitForResult: %v", err)
	}
	var parsed map[string]bool
	if err := json.Unmarshal(result.StructuredResultJSON, &parsed); err != nil {
		t.Fatalf("unmarshal structured result: %v", err)
	}
	if parsed["has_blocking_finding"] {
		t.Errorf("has_blocking_finding = true, want false")
	}
	if string(result.EventLog) != "starting openhands\ncloning repo" {
		t.Errorf("EventLog = %q", result.EventLog)
	}
}

func TestWaitForResult_CommandResult(t *testing.T) {
	dir := shortTempDir(t)
	udsPath := filepath.Join(dir, "vsock.sock")

	l, err := Listen(udsPath, 53, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn := dialGuest(t, udsPath+"_53")
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(`{"type":"event","line":"cloning org/repo@main"}` + "\n"))
		_, _ = conn.Write([]byte(`{"type":"event","line":"running tests"}` + "\n"))
		_, _ = conn.Write([]byte(`{"type":"command_result","exit_code":3}` + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := l.WaitForResult(ctx)
	if err != nil {
		t.Fatalf("WaitForResult: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3", result.ExitCode)
	}
	if result.StructuredResultJSON != nil {
		t.Errorf("StructuredResultJSON = %q, want nil for command mode", result.StructuredResultJSON)
	}
	if string(result.EventLog) != "cloning org/repo@main\nrunning tests" {
		t.Errorf("EventLog = %q", result.EventLog)
	}
}

func TestWaitForResult_GuestReportedError(t *testing.T) {
	dir := shortTempDir(t)
	udsPath := filepath.Join(dir, "vsock.sock")

	l, err := Listen(udsPath, 53, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn := dialGuest(t, udsPath+"_53")
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(`{"type":"error","message":"MMDS fetch failed: timeout"}` + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = l.WaitForResult(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWaitForResult_ClosedWithoutResult(t *testing.T) {
	dir := shortTempDir(t)
	udsPath := filepath.Join(dir, "vsock.sock")

	l, err := Listen(udsPath, 53, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	go func() {
		conn := dialGuest(t, udsPath+"_53")
		if conn == nil {
			return
		}
		_, _ = conn.Write([]byte(`{"type":"event","line":"openhands crashed"}` + "\n"))
		conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := l.WaitForResult(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if string(result.EventLog) != "openhands crashed" {
		t.Errorf("EventLog = %q, want event log preserved despite the error", result.EventLog)
	}
}

func TestWaitForResult_ContextCanceledBeforeConnect(t *testing.T) {
	dir := shortTempDir(t)
	udsPath := filepath.Join(dir, "vsock.sock")

	l, err := Listen(udsPath, 53, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = l.WaitForResult(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRedact(t *testing.T) {
	secrets := map[string]string{
		"FORGEJO_TOKEN": "sekret-token-123",
		"LLM_API_KEY":   "sk-abc-999",
		"EMPTY_SECRET":  "",
	}
	data := []byte(`{"type":"event","line":"git push https://x-access-token:sekret-token-123@host/org/repo LLM key sk-abc-999"}`)

	got := Redact(data, secrets)

	if bytes.Contains(got, []byte("sekret-token-123")) {
		t.Errorf("FORGEJO_TOKEN value leaked into redacted output: %s", got)
	}
	if bytes.Contains(got, []byte("sk-abc-999")) {
		t.Errorf("LLM_API_KEY value leaked into redacted output: %s", got)
	}
	if !bytes.Contains(got, []byte("[REDACTED:FORGEJO_TOKEN]")) || !bytes.Contains(got, []byte("[REDACTED:LLM_API_KEY]")) {
		t.Errorf("missing redaction markers: %s", got)
	}
}

func TestRedact_SymbolicReferenceLeftAlone(t *testing.T) {
	secrets := map[string]string{"FORGEJO_TOKEN": "sekret-token-123"}
	data := []byte(`echo "using \${FORGEJO_TOKEN} from env"`)

	got := Redact(data, secrets)

	if !bytes.Equal(got, data) {
		t.Errorf("symbolic reference should pass through untouched, got %s", got)
	}
}
