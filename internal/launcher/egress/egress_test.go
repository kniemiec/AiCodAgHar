package egress

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTapDeviceCommands(t *testing.T) {
	cfg := Config{TapDevice: "tap0", TapCIDR: "172.16.0.1/30", TapOwnerUID: 123150}
	cmds := TapDeviceCommands(cfg)

	if len(cmds) != 4 {
		t.Fatalf("got %d commands, want 4", len(cmds))
	}
	if !cmds[0].IgnoreError {
		t.Error("delete-if-exists command should have IgnoreError = true")
	}
	for i, c := range cmds[1:] {
		if c.IgnoreError {
			t.Errorf("command %d (%v) should not ignore errors", i+1, c.Argv)
		}
	}

	want := [][]string{
		{"ip", "link", "del", "tap0"},
		{"ip", "tuntap", "add", "dev", "tap0", "mode", "tap", "user", "123150"},
		{"ip", "addr", "add", "172.16.0.1/30", "dev", "tap0"},
		{"ip", "link", "set", "dev", "tap0", "up"},
	}
	for i, c := range cmds {
		if strings.Join(c.Argv, " ") != strings.Join(want[i], " ") {
			t.Errorf("command %d = %v, want %v", i, c.Argv, want[i])
		}
	}
}

func TestNftablesCommands(t *testing.T) {
	cmds := NftablesCommands("fc_launcher", "tap0")

	if len(cmds) != 4 {
		t.Fatalf("got %d commands, want 4", len(cmds))
	}
	if !cmds[0].IgnoreError || !cmds[1].IgnoreError {
		t.Error("create-table and create-chain commands should have IgnoreError = true")
	}
	if cmds[2].IgnoreError || cmds[3].IgnoreError {
		t.Error("flush and add-rule commands must not ignore errors")
	}

	rule := cmds[3].Argv
	if rule[0] != "nft" || rule[1] != "add" || rule[2] != "rule" {
		t.Errorf("rule command = %v, want it to start with 'nft add rule'", rule)
	}
	found := false
	for i, tok := range rule {
		if tok == "iifname" && i+1 < len(rule) && rule[i+1] == "tap0" {
			found = true
		}
	}
	if !found {
		t.Errorf("rule command = %v, want an 'iifname tap0' pair", rule)
	}
	if rule[len(rule)-1] != "drop" {
		t.Errorf("rule command = %v, want it to end with 'drop'", rule)
	}
}

func TestProxyAllowLine(t *testing.T) {
	if got := ProxyAllowLine("172.16.0.0/24"); got != "Allow 172.16.0.0/24" {
		t.Errorf("ProxyAllowLine = %q", got)
	}
}

func TestEnsureProxyAllowsSubnet_AlreadyPresentSkipsRestart(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "tinyproxy.conf")
	if err := os.WriteFile(confPath, []byte("Port 8888\nAllow 172.16.0.0/24\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	restarted := false
	err := ensureProxyAllowsSubnet(Config{
		ProxyConfigPath:  confPath,
		ProxyServiceName: "tinyproxy",
		GuestSubnet:      "172.16.0.0/24",
	}, func(argv []string) error {
		restarted = true
		return nil
	})
	if err != nil {
		t.Fatalf("ensureProxyAllowsSubnet: %v", err)
	}
	if restarted {
		t.Error("restarted the proxy even though the Allow line was already present")
	}
}

func TestEnsureProxyAllowsSubnet_AppendsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "tinyproxy.conf")
	original := "Port 8888\n"
	if err := os.WriteFile(confPath, []byte(original), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var gotArgv []string
	err := ensureProxyAllowsSubnet(Config{
		ProxyConfigPath:  confPath,
		ProxyServiceName: "tinyproxy",
		GuestSubnet:      "172.16.0.0/24",
	}, func(argv []string) error {
		gotArgv = argv
		return nil
	})
	if err != nil {
		t.Fatalf("ensureProxyAllowsSubnet: %v", err)
	}

	if strings.Join(gotArgv, " ") != "systemctl restart tinyproxy" {
		t.Errorf("restart command = %v", gotArgv)
	}

	updated, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read updated conf: %v", err)
	}
	if !strings.Contains(string(updated), "Allow 172.16.0.0/24") {
		t.Errorf("conf file missing Allow line: %s", updated)
	}
	if !strings.HasPrefix(string(updated), original) {
		t.Errorf("conf file should preserve original content, got: %s", updated)
	}

	matches, _ := filepath.Glob(confPath + ".bak.*")
	if len(matches) != 1 {
		t.Errorf("expected exactly one backup file, found %v", matches)
	}
}

func TestEnsureProxyAllowsSubnet_RestartFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "tinyproxy.conf")
	if err := os.WriteFile(confPath, []byte("Port 8888\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	err := ensureProxyAllowsSubnet(Config{
		ProxyConfigPath:  confPath,
		ProxyServiceName: "tinyproxy",
		GuestSubnet:      "172.16.0.0/24",
	}, func(argv []string) error {
		return errors.New("systemctl: unit not found")
	})
	if err == nil {
		t.Fatal("expected an error when the restart command fails")
	}
}
