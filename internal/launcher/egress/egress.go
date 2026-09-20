// Package egress performs the static, single-invocation host-side egress
// setup design §10 describes: one tap device, a host-terminated forward
// proxy (tinyproxy) bound to the tap gateway IP with the guest subnet
// allowlisted, and one nftables FORWARD-chain drop+log rule for that tap
// device — no NAT/MASQUERADE. This is not new design work: it's the
// Firecracker PoC's own already-validated mechanism
// (docs/plans/firecracker-microvm-launch-poc.md, Step 2;
// ~/firecracker-poc/run/setup-network.sh), reused here as-is rather than
// re-derived.
//
// The guest's only legitimate path out is a direct connection to the
// host's own tap-gateway IP (the proxy port), which is host-terminated
// traffic flowing through the INPUT chain, never FORWARD; anything else
// necessarily traverses FORWARD and is unconditionally dropped+logged
// there — no NAT/MASQUERADE rule is needed at all.
//
// This is deliberately a *static* setup (§10's own title), not
// per-invocation: v1 runs one invocation at a time, so one long-lived tap
// device, proxy config, and nftables rule are set up once (Setup is
// idempotent — safe to call on every launcher process startup) and reused
// across every launch, exactly matching what the PoC itself observed in
// practice (Step 6: killing the guest does not tear the tap device down).
// Concurrent-invocation tap/proxy/nftables-rule pooling and turning
// GrantedScopes into a real per-invocation proxy allowlist are named
// follow-up work (design §15), not built here.
package egress

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Config is the fixed, PoC-matching addressing/naming this package
// operates on. Hardcoded by callers (cmd/container-launcher/launch.go),
// not exposed as new env-var configuration — design §10 frames this
// entire mechanism as a fixed, reused-as-is setup, the same treatment
// already given to the boot_args/vcpu/memory/guestCID constants.
type Config struct {
	TapDevice   string // e.g. "tap0"
	TapCIDR     string // host-side address+mask on the tap device, e.g. "172.16.0.1/30"
	TapOwnerUID int    // must be the jailed Firecracker process's own uid (jailer.Config.UID) — a tap device created for a different uid can't be attached without CAP_NET_ADMIN, which the jailed process doesn't have (design §4)

	GuestSubnet string // what the proxy allows, e.g. "172.16.0.0/24" — deliberately broader than TapCIDR, matching the PoC exactly

	ProxyConfigPath  string // e.g. "/etc/tinyproxy/tinyproxy.conf"
	ProxyServiceName string // e.g. "tinyproxy", restarted via systemctl after an Allow line is added

	NftTable string // e.g. "fc_launcher" — a dedicated table, so this rule can't collide with unrelated host firewall config
}

// Command is one host command Setup runs, plus whether its failure is
// expected/ignorable — the idempotent-recreate pattern
// ~/firecracker-poc/run/setup-network.sh itself uses for a subset of its
// steps ("|| true" in the original script), not a generic "ignore every
// error" policy: only a command actually safe to fail on a retry gets
// IgnoreError.
type Command struct {
	Argv        []string
	IgnoreError bool
}

// DefaultConfig returns the fixed, PoC-matching addressing/naming this
// launcher always uses (design §10) — tapOwnerUID is the only value that
// varies per deployment (the jailer's own configured uid,
// LAUNCHER_JAILER_UID): the tap device must be owned by whichever uid the
// jailed Firecracker process actually runs as.
func DefaultConfig(tapOwnerUID int) Config {
	return Config{
		TapDevice:        "tap0",
		TapCIDR:          "172.16.0.1/30",
		TapOwnerUID:      tapOwnerUID,
		GuestSubnet:      "172.16.0.0/24",
		ProxyConfigPath:  "/etc/tinyproxy/tinyproxy.conf",
		ProxyServiceName: "tinyproxy",
		NftTable:         "fc_launcher",
	}
}

// Setup performs the tap device, nftables, and proxy-config steps, in
// that order (idempotent — see the package doc comment). Every step here
// mutates live host state and needs root, exactly like jailer itself, so
// this is expected to run once at launcher process startup, not per
// invocation or under go test.
func Setup(cfg Config) error {
	for _, c := range TapDeviceCommands(cfg) {
		if err := run(c.Argv); err != nil && !c.IgnoreError {
			return fmt.Errorf("egress: tap device: %w", err)
		}
	}

	for _, c := range NftablesCommands(cfg.NftTable, cfg.TapDevice) {
		if err := run(c.Argv); err != nil && !c.IgnoreError {
			return fmt.Errorf("egress: nftables: %w", err)
		}
	}

	if err := ensureProxyAllowsSubnet(cfg, run); err != nil {
		return fmt.Errorf("egress: proxy: %w", err)
	}
	return nil
}

// TapDeviceCommands returns the command sequence Setup runs to
// (re)create the tap device, in order — pure and unit-testable without
// root, mirroring internal/launcher/jailer.Config.Argv(). The first
// command (delete-if-exists) is allowed to fail: it's the
// idempotent-recreate pattern setup-network.sh itself uses, not an error
// on a fresh host where the device doesn't exist yet.
func TapDeviceCommands(cfg Config) []Command {
	return []Command{
		{Argv: []string{"ip", "link", "del", cfg.TapDevice}, IgnoreError: true},
		{Argv: []string{"ip", "tuntap", "add", "dev", cfg.TapDevice, "mode", "tap", "user", strconv.Itoa(cfg.TapOwnerUID)}},
		{Argv: []string{"ip", "addr", "add", cfg.TapCIDR, "dev", cfg.TapDevice}},
		{Argv: []string{"ip", "link", "set", "dev", cfg.TapDevice, "up"}},
	}
}

// nftLogPrefix mirrors setup-network.sh's own rule text (with a
// launcher-specific prefix; "fc-poc-egress-blocked:" was the PoC's own
// throwaway value) — nft's rule grammar wants the log prefix as a quoted
// string literal, quotes included, even as a single argv element (no
// shell is involved in exec.Command, so nothing else supplies them).
const nftLogPrefix = `"container-launcher-egress-blocked: "`

// NftablesCommands returns the command sequence Setup runs to
// (re)install the FORWARD-chain drop+log rule for tapDevice, in order —
// pure and unit-testable without root. The first two commands (create
// table, create chain) are allowed to fail on every call after the
// first: nft has no "create if not exists" primitive for these, so
// setup-network.sh's own idempotency pattern is reused as-is. The flush
// and the rule itself must succeed for real — a failure there means the
// egress block isn't actually in place.
func NftablesCommands(table, tapDevice string) []Command {
	return []Command{
		{Argv: []string{"nft", "add", "table", "inet", table}, IgnoreError: true},
		{Argv: []string{"nft", "--", "add", "chain", "inet", table, "forward", "{ type filter hook forward priority 0; policy accept; }"}, IgnoreError: true},
		{Argv: []string{"nft", "flush", "chain", "inet", table, "forward"}},
		{Argv: []string{"nft", "add", "rule", "inet", table, "forward", "iifname", tapDevice, "log", "prefix", nftLogPrefix, "drop"}},
	}
}

// ProxyAllowLine is the tinyproxy.conf line that allowlists the guest
// subnet — a pure helper so ensureProxyAllowsSubnet's idempotency check
// and the actual line it appends can never drift apart.
func ProxyAllowLine(guestSubnet string) string {
	return "Allow " + guestSubnet
}

// ensureProxyAllowsSubnet appends an Allow line for cfg.GuestSubnet to the
// proxy's config file and restarts it via runner — but only if that line
// isn't already there (setup-network.sh's own idempotency check, reused
// as-is, backup file included). runner is injected (rather than always
// calling run directly) so the restart step — the one part of this
// package that can't safely execute in a unit test — is fakeable; Setup
// passes the real run.
func ensureProxyAllowsSubnet(cfg Config, runner func([]string) error) error {
	data, err := os.ReadFile(cfg.ProxyConfigPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfg.ProxyConfigPath, err)
	}

	allowLine := ProxyAllowLine(cfg.GuestSubnet)
	if bytes.Contains(data, []byte(allowLine)) {
		return nil
	}

	backupPath := fmt.Sprintf("%s.bak.%d", cfg.ProxyConfigPath, time.Now().Unix())
	if err := os.WriteFile(backupPath, data, 0o644); err != nil {
		return fmt.Errorf("backup %s: %w", cfg.ProxyConfigPath, err)
	}

	updated := append(append([]byte{}, data...), []byte("\n"+allowLine+"\n")...)
	if err := os.WriteFile(cfg.ProxyConfigPath, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", cfg.ProxyConfigPath, err)
	}

	return runner([]string{"systemctl", "restart", cfg.ProxyServiceName})
}

func run(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
