package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/launcher/egress"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/fcapi"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/jailer"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/mmds"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/vsock"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
)

// The boot itself is still a single hardcoded payload — the exact
// boot_args/vcpu/memory shape the Firecracker PoC validated
// (docs/plans/firecracker-microvm-launch-poc.md, Steps 1 & 5). Env (the
// caller's real credentials/target info) is injected for real via MMDS
// (design §7), and — when task or command is non-empty, i.e. an
// agent-stage prompt or a deterministic-step command was rendered — so is
// that value itself, plus (task only) the launcher's own LLM credentials.
const (
	bootArgs       = "console=ttyS0 reboot=k panic=1"
	vcpuCount      = 2
	memSizeMib     = 1024
	cgroupVersion  = 2
	cgroupWeight   = "cpu.weight=100"
	apiSockInJail  = "/run/firecracker.socket"
	vsockUDSInJail = "/vsock.sock"
	// guestCID must be >= 3 (0/1 are hypervisor-reserved, 2 is always the
	// host). v1 runs one invocation at a time, so a single fixed CID is
	// enough — concurrency-safe allocation is follow-up work.
	guestCID  = 3
	vsockPort = 53

	// netIfaceID/guestMAC are the fixed Firecracker-side identifiers for
	// the one network interface every invocation gets (design §10). v1
	// runs one invocation at a time — like guestCID above — so fixed
	// values are enough; guestMAC matches the PoC's own convention of
	// encoding the guest's fixed IP (172.16.0.2) into the low 4 octets
	// (confirmed against the real API by the PoC's Step 2,
	// ~/firecracker-poc/run/step2-console.log).
	netIfaceID = "net1"
	guestMAC   = "06:00:AC:10:00:02"

	kernelFilename = "vmlinux"
	rootfsFilename = "rootfs.ext4"

	socketWaitTimeout  = 10 * time.Second
	socketPollInterval = 100 * time.Millisecond
	// bootGracePeriod is the completion signal for a launch with no
	// rendered task (no vsock event stream to wait on): "the jailed
	// process is still alive this long after InstanceStart".
	bootGracePeriod = 5 * time.Second
)

// resolveTimeout applies design §8's caller-override-with-ceiling rule:
// requestedSeconds <= 0 means "no caller-supplied timeout" — true of both
// executors today — so the launcher's own configured ceiling
// (defaultSeconds, from LAUNCHER_DEFAULT_TIMEOUT_SECONDS) always applies. A
// positive caller-supplied value overrides it.
func resolveTimeout(requestedSeconds int32, defaultSeconds int) time.Duration {
	if requestedSeconds > 0 {
		return time.Duration(requestedSeconds) * time.Second
	}
	return time.Duration(defaultSeconds) * time.Second
}

// statusForCancellation distinguishes design §6's "killed" status from a
// generic "error" when a wait loop's ctx is done: ctx is expected to be
// (a descendant of) the context.WithCancelCause context server.go's Launch
// handler builds per invocation, whose cause is set to errInvocationKilled
// specifically when Kill pre-empted it — any other cause (the caller's own
// deadline/cancellation) still reads as "error".
func statusForCancellation(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errInvocationKilled) {
		return "killed"
	}
	return "error"
}

// firecrackerLauncher is vmLauncher's production implementation: boots one
// jailed Firecracker microVM from the golden image. With a rendered task
// (agent-stage) or command (deterministic-step) — the two are mutually
// exclusive — it waits for the guest wrapper's vsock event stream to report
// a completion record before tearing down; with neither, it falls back to
// step 1/2's grace-period boot check.
type firecrackerLauncher struct {
	cfg       config
	egressCfg egress.Config
}

func newFirecrackerLauncher(cfg config) *firecrackerLauncher {
	return &firecrackerLauncher{cfg: cfg, egressCfg: egress.DefaultConfig(cfg.JailerUID)}
}

func (l *firecrackerLauncher) Launch(ctx context.Context, invocationID string, env map[string]string, task, command string, timeoutSeconds int32) (bootOutcome, error) {
	log := slog.With("invocation_id", invocationID)
	bootStart := time.Now()
	// stage logs one completed boot step at debug. Failures aren't logged
	// here: they're returned and logged once by server.go's Launch.
	stage := func(name string, attrs ...any) {
		log.DebugContext(ctx, "boot stage done", append([]any{"stage", name, "elapsed", time.Since(bootStart)}, attrs...)...)
	}

	jailCfg := jailer.Config{
		JailerBinary:      l.cfg.JailerBinary,
		FirecrackerBinary: l.cfg.FirecrackerBinary,
		ID:                invocationID,
		UID:               l.cfg.JailerUID,
		GID:               l.cfg.JailerGID,
		ChrootBaseDir:     l.cfg.ChrootBaseDir,
		CgroupVersion:     cgroupVersion,
		CgroupParams:      []string{cgroupWeight},
		FirecrackerArgs:   []string{"--api-sock", apiSockInJail},
	}
	if err := jailCfg.Validate(); err != nil {
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: %w", err)
	}

	// Fail fast on a ChrootBaseDir/invocation-id combination that would
	// produce an unusable Unix domain socket path (jailer.SocketPath's own
	// doc comment) — checked before starting jailer at all, since no
	// amount of waiting fixes a path that's already too long.
	socketPath, err := jailCfg.SocketPath(apiSockInJail)
	if err != nil {
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: %w", err)
	}
	vsockHostPath, err := jailCfg.SocketPath(vsockUDSInJail)
	if err != nil {
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: %w", err)
	}

	// jailer canonicalizes --chroot-base-dir and errors if it doesn't
	// already exist — it does not create it itself (PoC Step 5 finding).
	if err := os.MkdirAll(l.cfg.ChrootBaseDir, 0o755); err != nil {
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: chroot base dir: %w", err)
	}

	jailDir := filepath.Join(l.cfg.ChrootBaseDir, jailCfg.ExecBasename(), invocationID)
	defer teardown(jailDir)
	stage("jail-config", "jail_dir", jailDir, "api_socket", socketPath)

	cmd := jailCfg.Command(ctx)
	jailerLogPath := filepath.Join(os.TempDir(), "container-launcher-"+invocationID+".log")
	logFile, err := os.Create(jailerLogPath)
	if err != nil {
		log.WarnContext(ctx, "could not create jailer log file; jailer output discarded", "path", jailerLogPath, "error", err)
	}
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}

	if err := cmd.Start(); err != nil {
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: start jailer: %w", err)
	}
	stage("jailer-started", "pid", cmd.Process.Pid, "jailer_log", jailerLogPath)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	killAndReap := func() {
		log.DebugContext(ctx, "killing jailed firecracker process", "elapsed", time.Since(bootStart))
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
		<-exited
	}

	waitCtx, cancel := context.WithTimeout(ctx, socketWaitTimeout)
	err = fcapi.WaitForSocket(waitCtx, socketPath, socketPollInterval)
	cancel()
	if err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: waiting for API socket: %w (see %s)", err, jailerLogPath)
	}
	stage("api-socket-ready")

	if err := stageImages(jailCfg.JailRoot(), l.cfg.ImageRoot, l.cfg.JailerUID, l.cfg.JailerGID); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: staging images: %w", err)
	}
	stage("images-staged", "image_root", l.cfg.ImageRoot)

	fc := fcapi.NewClient(socketPath)
	if err := fc.PutBootSource(ctx, fcapi.BootSourceConfig{
		KernelImagePath: "/" + kernelFilename,
		BootArgs:        bootArgs,
	}); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: boot-source: %w", err)
	}
	if err := fc.PutDrive(ctx, fcapi.DriveConfig{
		DriveID:      "rootfs",
		PathOnHost:   "/" + rootfsFilename,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: drives: %w", err)
	}
	if err := fc.PutMachineConfig(ctx, fcapi.MachineConfig{
		VCPUCount:  vcpuCount,
		MemSizeMib: memSizeMib,
	}); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: machine-config: %w", err)
	}
	stage("boot-source-drive-machine-config", "vcpus", vcpuCount, "mem_mib", memSizeMib)

	// Every invocation gets the one static tap device egress.Setup already
	// provisioned at launcher startup (design §10) — attached
	// unconditionally, not just for task/command modes, so a bare-boot
	// payload gets the same egress posture as a real one.
	if err := fc.PutNetworkInterface(ctx, fcapi.NetworkInterfaceConfig{
		IfaceID:     netIfaceID,
		GuestMAC:    guestMAC,
		HostDevName: l.egressCfg.TapDevice,
	}); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: network-interfaces: %w", err)
	}
	stage("network-interface", "tap_device", l.egressCfg.TapDevice)

	var vsockListener *vsock.Listener
	if task != "" || command != "" {
		vsockListener, err = vsock.Listen(vsockHostPath, vsockPort, l.cfg.JailerUID, l.cfg.JailerGID)
		if err != nil {
			killAndReap()
			return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: vsock listen: %w", err)
		}
		defer vsockListener.Close()

		if err := fc.PutVsock(ctx, fcapi.VsockConfig{GuestCID: guestCID, UDSPath: vsockUDSInJail}); err != nil {
			killAndReap()
			return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: vsock config: %w", err)
		}
		stage("vsock-listening", "uds", vsockHostPath, "guest_cid", guestCID, "port", vsockPort)
	}

	// MMDS must carry the guest's credentials/task before InstanceStart —
	// the guest's very first boot-time fetch needs to see them (design §7).
	// netIfaceID must be in /mmds/config's own network_interfaces list for
	// the just-attached NIC to actually serve MMDS requests — confirmed
	// against the real API by the PoC's Step 3; there is no separate
	// per-NIC "allow_mmds_requests" field.
	if err := mmds.Configure(ctx, fc, []string{netIfaceID}); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: mmds config: %w", err)
	}
	mmdsData := env
	switch {
	case task != "":
		mmdsData = mergeAgentEnv(env, task, l.cfg.LLMAPIKey, l.cfg.LLMModel)
	case command != "":
		mmdsData = mergeCommandEnv(env, command)
	}
	if err := mmds.Inject(ctx, fc, mmdsData); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: mmds inject: %w", err)
	}
	stage("mmds-injected", "keys", logging.Keys(mmdsData))

	if err := fc.InstanceStart(ctx); err != nil {
		killAndReap()
		return bootOutcome{status: "error"}, fmt.Errorf("container-launcher: instance-start: %w", err)
	}
	log.InfoContext(ctx, "microVM started", "boot_elapsed", time.Since(bootStart), "jailer_log", jailerLogPath,
		"mode", launchMode(task, command))

	if task == "" && command == "" {
		select {
		case err := <-exited:
			// Crashed (or shut itself down) before the grace period
			// elapsed — not a clean completion signal for a payload that
			// should just sit at a boot prompt.
			return bootOutcome{exitCode: 1, status: "error"}, fmt.Errorf("container-launcher: guest process exited during boot: %v", err)
		case <-time.After(bootGracePeriod):
		case <-ctx.Done():
			killAndReap()
			return bootOutcome{status: statusForCancellation(ctx)}, context.Cause(ctx)
		}

		killAndReap()
		return bootOutcome{exitCode: 0, status: "completed"}, nil
	}

	timeout := resolveTimeout(timeoutSeconds, l.cfg.DefaultTimeoutSeconds)
	log.InfoContext(ctx, "waiting for guest completion", "timeout", timeout)
	if task != "" {
		return l.waitForAgentCompletion(ctx, log, env, vsockListener, exited, killAndReap, timeout)
	}
	return l.waitForCommandCompletion(ctx, log, env, vsockListener, exited, killAndReap, timeout)
}

func launchMode(task, command string) string {
	switch {
	case task != "":
		return "agent"
	case command != "":
		return "command"
	default:
		return "bare-boot"
	}
}

// waitForAgentCompletion waits for the guest wrapper's structured-result
// record over vsock, a guest process crash, an explicit Kill, or timeout
// (resolveTimeout's result, design §8) — whichever comes first — then
// tears the microVM down. Every return path runs deriveVsockOutcome's
// fail-closed status/structured-result mapping (design §6) and
// vsock.Redact's literal-secret scrub (design §7) before the result leaves
// this function — nothing captured here reaches ExtractLogsResponse
// un-redacted.
func (l *firecrackerLauncher) waitForAgentCompletion(ctx context.Context, log *slog.Logger, env map[string]string, vsockListener *vsock.Listener, exited <-chan error, killAndReap func(), timeout time.Duration) (bootOutcome, error) {
	waitCtx, waitCancel := context.WithCancel(ctx)
	defer waitCancel()

	type waitOutcome struct {
		result vsock.Result
		err    error
	}
	resultCh := make(chan waitOutcome, 1)
	go func() {
		result, err := vsockListener.WaitForResult(waitCtx)
		resultCh <- waitOutcome{result, err}
	}()

	var status string
	var exitCode int32
	var structuredResultJSON []byte
	var stdout []byte
	var outErr error

	select {
	case r := <-resultCh:
		log.InfoContext(ctx, "guest completion record received", "vsock_error", r.err, "event_log_bytes", len(r.result.EventLog))
		killAndReap()
		stdout = r.result.EventLog
		status, exitCode, structuredResultJSON = deriveVsockOutcome(r.result, r.err)
		if status != "completed" {
			if r.err != nil {
				outErr = fmt.Errorf("container-launcher: %w", r.err)
			} else {
				outErr = fmt.Errorf("container-launcher: guest structured result missing or malformed")
			}
		}

	case err := <-exited:
		log.WarnContext(ctx, "guest process exited before completion", "error", err)
		waitCancel()
		<-resultCh
		status, exitCode, structuredResultJSON = "error", 1, []byte(failClosedStructuredResult)
		outErr = fmt.Errorf("container-launcher: guest process exited before completion: %v", err)

	case <-time.After(timeout):
		log.WarnContext(ctx, "timed out waiting for guest completion", "timeout", timeout)
		waitCancel()
		<-resultCh
		killAndReap()
		status, structuredResultJSON = "timed_out", []byte(failClosedStructuredResult)
		outErr = fmt.Errorf("container-launcher: timed out waiting for guest completion after %s", timeout)

	case <-ctx.Done():
		log.WarnContext(ctx, "invocation cancelled while waiting for guest", "cause", context.Cause(ctx))
		waitCancel()
		<-resultCh
		killAndReap()
		status, structuredResultJSON = statusForCancellation(ctx), []byte(failClosedStructuredResult)
		outErr = context.Cause(ctx)
	}

	secrets := map[string]string{
		"FORGEJO_TOKEN": env["FORGEJO_TOKEN"],
		"LLM_API_KEY":   l.cfg.LLMAPIKey,
	}
	return bootOutcome{
		exitCode:             exitCode,
		status:               status,
		structuredResultJSON: vsock.Redact(structuredResultJSON, secrets),
		stdout:               vsock.Redact(stdout, secrets),
	}, outErr
}

// waitForCommandCompletion is waitForAgentCompletion's deterministic-step
// counterpart: the same completion race (guest completion record vs guest
// crash vs explicit Kill vs timeout), resolved through deriveCommandOutcome
// instead — the guest command's real exit code, propagated directly
// (design §6), not agent-stage's has_blocking_finding fail-closed default.
// There is no structured result in command mode, so
// bootOutcome.structuredResultJSON is left unset on every path.
func (l *firecrackerLauncher) waitForCommandCompletion(ctx context.Context, log *slog.Logger, env map[string]string, vsockListener *vsock.Listener, exited <-chan error, killAndReap func(), timeout time.Duration) (bootOutcome, error) {
	waitCtx, waitCancel := context.WithCancel(ctx)
	defer waitCancel()

	type waitOutcome struct {
		result vsock.Result
		err    error
	}
	resultCh := make(chan waitOutcome, 1)
	go func() {
		result, err := vsockListener.WaitForResult(waitCtx)
		resultCh <- waitOutcome{result, err}
	}()

	var status string
	var exitCode int32
	var stdout []byte
	var outErr error

	select {
	case r := <-resultCh:
		log.InfoContext(ctx, "guest completion record received", "vsock_error", r.err, "event_log_bytes", len(r.result.EventLog))
		killAndReap()
		stdout = r.result.EventLog
		status, exitCode = deriveCommandOutcome(r.result, r.err)
		if status != "completed" {
			if r.err != nil {
				outErr = fmt.Errorf("container-launcher: %w", r.err)
			} else {
				outErr = fmt.Errorf("container-launcher: guest command result missing")
			}
		}

	case err := <-exited:
		log.WarnContext(ctx, "guest process exited before completion", "error", err)
		waitCancel()
		<-resultCh
		status, exitCode = "error", 1
		outErr = fmt.Errorf("container-launcher: guest process exited before completion: %v", err)

	case <-time.After(timeout):
		log.WarnContext(ctx, "timed out waiting for guest completion", "timeout", timeout)
		waitCancel()
		<-resultCh
		killAndReap()
		status, exitCode = "timed_out", 1
		outErr = fmt.Errorf("container-launcher: timed out waiting for guest completion after %s", timeout)

	case <-ctx.Done():
		log.WarnContext(ctx, "invocation cancelled while waiting for guest", "cause", context.Cause(ctx))
		waitCancel()
		<-resultCh
		killAndReap()
		status, exitCode = statusForCancellation(ctx), 1
		outErr = context.Cause(ctx)
	}

	secrets := map[string]string{"FORGEJO_TOKEN": env["FORGEJO_TOKEN"]}
	return bootOutcome{
		exitCode: exitCode,
		status:   status,
		stdout:   vsock.Redact(stdout, secrets),
	}, outErr
}

// mergeCommandEnv layers the resolved fixed guest command on top of the
// caller's Env — the guest wrapper clones TARGET_REPO@BRANCH using the
// caller's own credential (already present in env) and then runs this
// command inside that clone (design §5's "command" mode).
func mergeCommandEnv(env map[string]string, command string) map[string]string {
	merged := make(map[string]string, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged["COMMAND"] = command
	return merged
}

// mergeAgentEnv layers the rendered task and the launcher's own LLM
// credentials on top of the caller's Env, all in the same UPPER_SNAKE_CASE
// key style — the guest wrapper exports every key but TASK straight into
// OpenHands' environment (design §7).
func mergeAgentEnv(env map[string]string, task, llmAPIKey, llmModel string) map[string]string {
	merged := make(map[string]string, len(env)+3)
	for k, v := range env {
		merged[k] = v
	}
	merged["TASK"] = task
	if llmAPIKey != "" {
		merged["LLM_API_KEY"] = llmAPIKey
	}
	if llmModel != "" {
		merged["LLM_MODEL"] = llmModel
	}
	return merged
}

// stageImages copies — never hardlinks — the golden kernel and rootfs into
// the jail and chowns the copies to the jailed uid/gid so the unprivileged
// Firecracker process can read them (design §9's "plain full-copy per
// invocation" decision). The PoC's Step 5 found that hardlinking a shared
// golden image and then chowning it mutates the original file's ownership
// too, since a hardlink shares one inode with its source — copying avoids
// that entirely.
func stageImages(jailRoot, imageRoot string, uid, gid int) error {
	if err := copyAndChown(filepath.Join(imageRoot, kernelFilename), filepath.Join(jailRoot, kernelFilename), uid, gid); err != nil {
		return err
	}
	if err := copyAndChown(filepath.Join(imageRoot, rootfsFilename), filepath.Join(jailRoot, rootfsFilename), uid, gid); err != nil {
		return err
	}
	return nil
}

func copyAndChown(src, dst string, uid, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Chown(uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", dst, err)
	}
	return nil
}

// teardown removes this invocation's jail directory tree — deferred
// unconditionally right after Launch computes jailDir, so it runs on every
// return path (design §8's "basic teardown sequence": the API socket, the
// vsock UDS, and the per-invocation rootfs/kernel copies all live under
// jailDir, so removing it covers all of those in one step; process
// termination itself is killAndReap's job, called before teardown fires on
// every path that started one). Tap-device deletion is deliberately not
// here: design §10 frames egress as a *static* setup, not per-invocation
// (v1 runs one invocation at a time), so the tap device egress.Setup
// created at launcher startup is meant to outlive any single invocation —
// matching what the Firecracker PoC itself observed (killing the guest
// does not tear the tap device down). Per-invocation tap/proxy/rule
// pooling and teardown is named follow-up work (design §15), not this.
// Orphan-sweep-on-restart and retry-with-backoff hardening (design §8) are
// likewise named as follow-up work, not needed to prove one real boot —
// this is happy-path-and-explicit-kill-only cleanup, same as the kill
// switch itself. jailDir is
// <chroot-base-dir>/<exec-basename>/<invocation-id>, one level above the
// "root" subdirectory jailer itself creates.
func teardown(jailDir string) {
	if err := os.RemoveAll(jailDir); err != nil {
		slog.Warn("teardown: remove jail dir failed", "jail_dir", jailDir, "error", err)
		return
	}
	slog.Debug("teardown: jail dir removed", "jail_dir", jailDir)
}
