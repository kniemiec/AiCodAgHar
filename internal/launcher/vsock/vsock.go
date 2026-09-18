// Package vsock captures the guest wrapper's structured event stream over
// Firecracker's vsock device (design §7) — decoupled from the egress path
// (still unwired as of this step) so a network-level failure during the
// run doesn't also blind the host-side observer, matching the Firecracker
// PoC's Step 4 finding.
//
// Wire protocol (newline-delimited JSON, one envelope per line), shared
// with internal/launcher/guest/guest_wrapper.py:
//
//	{"type": "event", "line": "<raw openhands --json line, or a command's raw output line>"}
//	{"type": "structured_result", "data": {...}}          // agent-stage mode
//	{"type": "command_result", "exit_code": <int>}        // command mode
//	{"type": "error", "message": "<what went wrong>"}
//
// Firecracker's vsock device does not use its own configured uds_path
// directly for a *guest-initiated* connection — it opens an outbound Unix
// connection to "<uds_path>_<port>", which the host must already have
// bound and listening before the guest calls connect() (PoC Step 4
// finding, reused directly here). Listen resolves that path.
package vsock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

type envelope struct {
	Type     string          `json:"type"`
	Line     string          `json:"line,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Message  string          `json:"message,omitempty"`
	ExitCode *int32          `json:"exit_code,omitempty"`
}

// Result is what WaitForResult reports once the guest's connection closes,
// whether or not it ever sent a completion envelope (structured_result or
// command_result).
type Result struct {
	// StructuredResultJSON is nil if the guest never sent a structured_result
	// envelope (agent-stage mode).
	StructuredResultJSON []byte
	// ExitCode is nil if the guest never sent a command_result envelope
	// (command mode).
	ExitCode *int32
	// EventLog is the guest's raw event lines, newline-joined, verbatim —
	// pre-redaction. Callers must run Redact on this (and on
	// StructuredResultJSON) before returning either outside the launcher.
	EventLog []byte
}

// Listener is a host-side vsock endpoint for exactly one invocation's
// guest connection.
type Listener struct {
	path string
	lis  net.Listener
}

// Listen binds the host-side socket at "<udsPath>_<port>" and chowns it to
// uid/gid — the jailed Firecracker process's own uid/gid (jailer.Config.UID/
// GID), matching how jailer itself chowns the device nodes it mknods inside
// the chroot (/dev/kvm, /dev/net/tun). Without this, the socket file is
// owned by whoever this process runs as (typically root, since jailer
// itself needs root), and Firecracker's own connect(2) to it — issued as
// the jailed, unprivileged uid on the guest's behalf for a guest-initiated
// vsock connection — fails with EACCES (AF_UNIX connect requires write
// permission on the target socket file for the connecting process): the
// guest's own connect() attempts keep failing and retry until they give up,
// which surfaces host-side as Accept() simply never firing, not as any
// visible error. Must be called before InstanceStart, so Firecracker has
// somewhere to connect the guest's vsock stream to once it boots.
func Listen(udsPath string, port uint32, uid, gid int) (*Listener, error) {
	path := fmt.Sprintf("%s_%d", udsPath, port)
	_ = os.Remove(path) // stale socket from an earlier, uncleanly-torn-down invocation
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("vsock: listen on %s: %w", path, err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		lis.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("vsock: chown %s to %d:%d: %w", path, uid, gid, err)
	}
	return &Listener{path: path, lis: lis}, nil
}

// Close stops accepting and removes the socket file. Also unblocks any
// WaitForResult call still waiting on Accept — callers should defer this
// unconditionally right after Listen succeeds, not only on the happy path.
func (l *Listener) Close() error {
	err := l.lis.Close()
	_ = os.Remove(l.path)
	return err
}

// WaitForResult accepts the guest's one connection, decodes its envelope
// stream, and returns once that connection closes. It returns a non-nil
// error if the guest wrapper reported an "error" envelope, if the stream
// closed without ever sending a completion envelope (structured_result for
// agent-stage mode, command_result for command mode), or if ctx is done
// first — in every case, Result still carries whatever event log bytes
// were captured before the failure.
func (l *Listener) WaitForResult(ctx context.Context) (Result, error) {
	connCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := l.lis.Accept()
		if err != nil {
			errCh <- err
			return
		}
		connCh <- conn
	}()

	var conn net.Conn
	select {
	case conn = <-connCh:
	case err := <-errCh:
		return Result{}, fmt.Errorf("vsock: accept: %w", err)
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer conn.Close()

	type readOutcome struct {
		result Result
		err    error
	}
	readCh := make(chan readOutcome, 1)
	go func() {
		result, err := readEnvelopes(conn)
		readCh <- readOutcome{result, err}
	}()

	select {
	case r := <-readCh:
		return r.result, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Redact returns a copy of data with every occurrence of any non-empty
// secret value replaced by a fixed "[REDACTED:<label>]" marker — a
// mechanical, literal-byte scrub applied to every captured byte stream
// before it leaves the launcher (design §7), not a semantic or
// context-aware filter. Symbolic references (e.g. "${FORGEJO_TOKEN}" as a
// shell variable name) are not secret material and pass through untouched
// — only the literal secret bytes the launcher itself injected are ever
// scrubbed.
func Redact(data []byte, secrets map[string]string) []byte {
	for label, value := range secrets {
		if value == "" {
			continue
		}
		data = bytes.ReplaceAll(data, []byte(value), []byte("[REDACTED:"+label+"]"))
	}
	return data
}

func readEnvelopes(conn net.Conn) (Result, error) {
	var events [][]byte
	var structuredResult []byte
	var exitCode *int32
	var guestErrMsg string

	scanner := bufio.NewScanner(conn)
	// A single OpenHands event line or the structured-result JSON can
	// exceed bufio.Scanner's 64KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			// Not our envelope protocol — surface it as an opaque event
			// line rather than dropping it silently.
			events = append(events, append([]byte(nil), line...))
			continue
		}
		switch env.Type {
		case "event":
			events = append(events, []byte(env.Line))
		case "structured_result":
			structuredResult = append([]byte(nil), env.Data...)
		case "command_result":
			if env.ExitCode != nil {
				ec := *env.ExitCode
				exitCode = &ec
			}
		case "error":
			guestErrMsg = env.Message
		}
	}

	result := Result{StructuredResultJSON: structuredResult, ExitCode: exitCode, EventLog: bytes.Join(events, []byte("\n"))}

	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("vsock: read: %w", err)
	}
	if guestErrMsg != "" {
		return result, fmt.Errorf("vsock: guest wrapper reported an error: %s", guestErrMsg)
	}
	if structuredResult == nil && exitCode == nil {
		return result, fmt.Errorf("vsock: guest connection closed without a completion record (structured_result or command_result)")
	}
	return result, nil
}
