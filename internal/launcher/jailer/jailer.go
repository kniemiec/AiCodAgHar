// Package jailer builds the argv and resolves the host-side filesystem
// paths for invoking Firecracker under jailer (ADR-10): jailer drops
// privileges, chroots, and applies a cgroup before exec'ing into the
// Firecracker binary. jailer itself must run as root to do that setup; per
// design §4 the whole container-launcher process already runs privileged,
// so no separate privilege-escalation step happens here — callers just
// exec jailer directly via Config.Command.
package jailer

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

// validID matches jailer's own charset restriction for --id and keeps ID
// safe to embed directly into filesystem paths (no "/", no "..").
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config describes one jailer invocation. CgroupVersion should be 2 —
// jailer defaults to cgroup v1, which fails outright on any modern
// cgroup-v2-only host (the Firecracker PoC's Step 5 finding); this launcher
// always passes it explicitly rather than relying on jailer's default.
type Config struct {
	JailerBinary      string
	FirecrackerBinary string
	ID                string
	UID               int
	GID               int
	ChrootBaseDir     string
	CgroupVersion     int
	CgroupParams      []string
	FirecrackerArgs   []string
}

// ExecBasename is the last path component of FirecrackerBinary — jailer
// nests the jail under <chroot-base-dir>/<exec-basename>/<id>/.
func (c Config) ExecBasename() string {
	return filepath.Base(c.FirecrackerBinary)
}

// JailRoot is the chroot jailer creates for this invocation: the directory
// the jailed Firecracker process sees as "/". Resource files referenced in
// Firecracker API calls (kernel, rootfs) must be staged here first, using
// their in-jail path (e.g. "/vmlinux") in the API request body.
func (c Config) JailRoot() string {
	return filepath.Join(c.ChrootBaseDir, c.ExecBasename(), c.ID, "root")
}

// Argv builds jailer's own command-line arguments (not including the
// jailer binary path itself).
func (c Config) Argv() []string {
	args := []string{
		"--id", c.ID,
		"--exec-file", c.FirecrackerBinary,
		"--uid", strconv.Itoa(c.UID),
		"--gid", strconv.Itoa(c.GID),
		"--chroot-base-dir", c.ChrootBaseDir,
	}
	if c.CgroupVersion != 0 {
		args = append(args, "--cgroup-version", strconv.Itoa(c.CgroupVersion))
	}
	for _, p := range c.CgroupParams {
		args = append(args, "--cgroup", p)
	}
	args = append(args, "--")
	args = append(args, c.FirecrackerArgs...)
	return args
}

// Command builds the exec.Cmd for this invocation.
func (c Config) Command(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, c.JailerBinary, c.Argv()...)
}

// Validate checks the fields Command/JailRoot depend on are usable — in
// particular that ID is safe to embed directly into filesystem paths and
// jailer's own --id argument.
func (c Config) Validate() error {
	if c.JailerBinary == "" {
		return fmt.Errorf("jailer: JailerBinary is required")
	}
	if c.FirecrackerBinary == "" {
		return fmt.Errorf("jailer: FirecrackerBinary is required")
	}
	if c.ChrootBaseDir == "" {
		return fmt.Errorf("jailer: ChrootBaseDir is required")
	}
	if !validID.MatchString(c.ID) {
		return fmt.Errorf("jailer: invalid ID %q: must match %s", c.ID, validID)
	}
	return nil
}

// maxUnixSocketPathLen is Linux's sockaddr_un.sun_path capacity minus its
// terminating NUL byte (unix(7)) — a hard kernel limit, not something
// jailer/Firecracker can work around. The jailed process itself never hits
// this: from inside the chroot, the API/vsock socket path is tiny (e.g.
// "/run/firecracker.socket"). Only host-side code connecting in via the
// full, un-chrooted path (ChrootBaseDir joined with "firecracker/<id>/root/…")
// can exceed it — and when it does, every connection attempt fails with
// ENAMETOOLONG, which previously surfaced as a confusing 10-second
// WaitForSocket timeout instead of a clear, immediate error.
const maxUnixSocketPathLen = 107

// SocketPath joins JailRoot with relPath (e.g. "/run/firecracker.socket" or
// a vsock UDS path) and checks the result is short enough to use as a Unix
// domain socket address. Call this before starting jailer, not just before
// dialing — a ChrootBaseDir/ID combination that produces too long a path
// will never work no matter how long WaitForSocket waits.
func (c Config) SocketPath(relPath string) (string, error) {
	p := filepath.Join(c.JailRoot(), relPath)
	if len(p) > maxUnixSocketPathLen {
		return "", fmt.Errorf("jailer: socket path %q is %d bytes, exceeds the %d-byte Unix domain socket limit (unix(7)) — use a shorter --chroot-base-dir or invocation id", p, len(p), maxUnixSocketPathLen)
	}
	return p, nil
}
