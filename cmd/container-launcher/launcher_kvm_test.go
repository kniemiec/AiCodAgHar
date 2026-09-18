//go:build launcherkvm

package main

import (
	"context"
	"os"
	"testing"
	"time"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/egress"
)

// TestLaunchKVM_BareBootTrivialPayload is design §14's KVM-gated
// integration layer: a real jailed Firecracker microVM booting end-to-end
// through this package's own real code (jailer, fcapi, mmds, vsock, egress)
// — not hand-driven PoC scripts. Run via `make test-launcher-kvm`, never by
// plain `go test ./...`: the launcherkvm build tag excludes this file from
// the default build entirely, matching this repo's existing
// integration/e2e build-tag convention (internal/clients/gitclient's
// `integration` tag, internal/control-plane/e2e's `e2e` tag).
//
// This intentionally stays in-process — calling the real *launcherServer
// Go value directly rather than round-tripping through gRPC+mTLS. The
// property worth a KVM-gated test is that jailer/fcapi/mmds/vsock/egress
// actually boot a real microVM together; gRPC marshaling over that is
// already proven generically by mocks/launcher-mock's identical
// bootstrap and by server_test.go's fake-booter RPC-bookkeeping tests, so
// re-proving it here would only add a TLS/cert-provisioning dependency
// for no new coverage.
//
// This is the "Integration (real KVM)" row of design §14's testing-strategy
// table — deliberately the cheapest possible real boot, no MMDS/vsock/
// OpenHands involved. See TestLaunchKVM_AgentStageRealForgejo below for
// the "real OpenHands + real Forgejo" layer.
func TestLaunchKVM_BareBootTrivialPayload(t *testing.T) {
	requireKVMDevice(t)
	cfg := kvmTestConfig(t)
	srv := newLauncherServer(cfg)

	resp, err := srv.Launch(context.Background(), &launcherv1.LaunchRequest{
		InvocationId: "kvm-smoke-bare-boot",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q, want completed", resp.Status)
	}
	if resp.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", resp.ExitCode)
	}

	logs, err := srv.ExtractLogs(context.Background(), &launcherv1.ExtractLogsRequest{InvocationId: resp.InvocationId})
	if err != nil {
		t.Fatalf("ExtractLogs: %v", err)
	}
	t.Logf("stdout: %s", logs.Stdout)
}

// TestLaunchKVM_AgentStageRealForgejo drives one real OpenHands agent-stage
// invocation against a real Forgejo repo — not design §14's polished
// `make test-launcher-e2e` target (which doesn't exist yet,
// docs/components/container-launcher.md §8), but the manual diagnostic
// that stands in for it today: run it explicitly with
//
//	sudo -E "$(mise where go@1.25.10)/bin/go" test -tags launcherkvm \
//	  ./cmd/container-launcher/... -run TestLaunchKVM_AgentStageRealForgejo -v
//
// Needs everything TestLaunchKVM_BareBootTrivialPayload needs, plus:
//
//   - LAUNCHER_LLM_API_KEY / LAUNCHER_LLM_MODEL — real OpenHands credentials.
//   - FORGEJO_TOKEN, TARGET_REPO ("owner/name"), BRANCH — a real,
//     already-existing repo/branch the token can push to.
//   - FORGEJO_URL — the address the GUEST can reach, i.e. the tap gateway
//     (design §10), not localhost: "http://172.16.0.1:3000" for the PoC's
//     local Forgejo, never "http://localhost:3000" (that's only reachable
//     from the host).
//   - guest_wrapper.py actually baked into the golden image
//     (~/firecracker-poc/rootfs/bake-guest-wrapper.sh) — without it the
//     guest boots but never touches MMDS/vsock, and this test times out
//     with status "timed_out" instead of "completed".
//
// t.Log calls throughout are deliberate: this is meant to be read by a
// human deciding whether a real run actually worked, not just a pass/fail
// bit — status/exit_code/structured_result/stdout (already redacted, see
// internal/launcher/vsock.Redact) are all surfaced even on success.
func TestLaunchKVM_AgentStageRealForgejo(t *testing.T) {
	requireKVMDevice(t)
	cfg := kvmTestConfig(t)
	cfg.LLMAPIKey = requireTestEnv(t, "LAUNCHER_LLM_API_KEY")
	cfg.LLMModel = requireTestEnv(t, "LAUNCHER_LLM_MODEL")
	srv := newLauncherServer(cfg)

	// egress.DefaultConfig's fixed tap-gateway proxy address (design §10) —
	// the guest can only reach anything outside its own tap subnet,
	// Forgejo included, by going through this.
	const proxyURL = "http://172.16.0.1:8888"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	resp, err := srv.Launch(ctx, &launcherv1.LaunchRequest{
		InvocationId: "kvm-smoke-agent-stage",
		StepType:     stepTypeAgentStage,
		PayloadRef:   "implement",
		Env: map[string]string{
			"FORGEJO_URL":   requireTestEnv(t, "FORGEJO_URL"),
			"FORGEJO_TOKEN": requireTestEnv(t, "FORGEJO_TOKEN"),
			"TARGET_REPO":   requireTestEnv(t, "TARGET_REPO"),
			"BRANCH":        requireTestEnv(t, "BRANCH"),
			"HTTP_PROXY":    proxyURL,
			"HTTPS_PROXY":   proxyURL,
		},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Logf("status=%s exit_code=%d", resp.Status, resp.ExitCode)

	logs, err := srv.ExtractLogs(context.Background(), &launcherv1.ExtractLogsRequest{InvocationId: resp.InvocationId})
	if err != nil {
		t.Fatalf("ExtractLogs: %v", err)
	}
	t.Logf("structured_result: %s", logs.StructuredResultJson)
	t.Logf("stdout: %s", logs.Stdout)

	if resp.Status != "completed" {
		t.Errorf("Status = %q, want completed — see the logged stdout/structured_result above for why", resp.Status)
	}
}

// requireKVMDevice is defense-in-depth on top of the Makefile target's own
// /dev/kvm check — skips (not fails) so `go test -tags launcherkvm ./...`
// run directly, bypassing `make test-launcher-kvm`, still degrades
// gracefully instead of failing with a confusing jailer/Firecracker error
// deep inside a real boot attempt.
func requireKVMDevice(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("/dev/kvm not accessible (%v) — this test requires a Linux host with hardware virtualization and this user able to read/write it", err)
	}
	f.Close()
}

// kvmTestConfig builds a real config from the same env vars
// cmd/container-launcher's own loadConfig reads
// (docs/components/container-launcher.md §5), skipping (not failing) the
// test when one is unset — this target is meant to run only where a real
// jailer/Firecracker/golden-image environment
// (docs/plans/firecracker-microvm-launch-poc.md) has already been set up,
// mirroring how this repo's other *_integration_test.go files skip rather
// than fail on missing env.
//
// It also runs egress.Setup itself: these tests call *launcherServer
// in-process rather than through main(), so nothing else here provisions
// the static tap device/nftables rule/proxy config main.go normally sets
// up once at real-binary startup (design §10) — every invocation's
// unconditional network-interfaces attach (cmd/container-launcher/launch.go)
// would otherwise fail immediately, bare-boot payload included, since
// Firecracker opens the named tap device the moment it's attached.
func kvmTestConfig(t *testing.T) config {
	t.Helper()
	cfg := config{
		ChrootBaseDir:         requireTestEnv(t, "LAUNCHER_CHROOT_BASE_DIR"),
		ImageRoot:             requireTestEnv(t, "LAUNCHER_IMAGE_ROOT"),
		JailerBinary:          requireTestEnv(t, "LAUNCHER_JAILER_BINARY"),
		FirecrackerBinary:     requireTestEnv(t, "LAUNCHER_FIRECRACKER_BINARY"),
		JailerUID:             envInt("LAUNCHER_JAILER_UID", 123150),
		JailerGID:             envInt("LAUNCHER_JAILER_GID", 123150),
		DefaultTimeoutSeconds: envInt("LAUNCHER_DEFAULT_TIMEOUT_SECONDS", 1800),
	}
	if err := egress.Setup(egress.DefaultConfig(cfg.JailerUID)); err != nil {
		t.Fatalf("egress.Setup: %v (this test needs to run as root, same as jailer)", err)
	}
	return cfg
}

func requireTestEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set — see docs/components/container-launcher.md §5 for this test's env contract", name)
	}
	return v
}
