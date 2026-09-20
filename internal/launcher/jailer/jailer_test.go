package jailer

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJailRoot(t *testing.T) {
	c := Config{
		FirecrackerBinary: "/opt/bin/firecracker",
		ID:                "inv-123",
		ChrootBaseDir:     "/var/lib/launcher/jails",
	}
	got := c.JailRoot()
	want := filepath.Join("/var/lib/launcher/jails", "firecracker", "inv-123", "root")
	if got != want {
		t.Errorf("JailRoot() = %q, want %q", got, want)
	}
}

func TestArgv(t *testing.T) {
	c := Config{
		JailerBinary:      "/opt/bin/jailer",
		FirecrackerBinary: "/opt/bin/firecracker",
		ID:                "inv-123",
		UID:               123150,
		GID:               123150,
		ChrootBaseDir:     "/var/lib/launcher/jails",
		CgroupVersion:     2,
		CgroupParams:      []string{"cpu.weight=100"},
		FirecrackerArgs:   []string{"--api-sock", "/run/firecracker.socket"},
	}
	want := []string{
		"--id", "inv-123",
		"--exec-file", "/opt/bin/firecracker",
		"--uid", "123150",
		"--gid", "123150",
		"--chroot-base-dir", "/var/lib/launcher/jails",
		"--cgroup-version", "2",
		"--cgroup", "cpu.weight=100",
		"--",
		"--api-sock", "/run/firecracker.socket",
	}
	got := c.Argv()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Argv() =\n%v\nwant\n%v", got, want)
	}
}

func TestValidate(t *testing.T) {
	base := Config{
		JailerBinary:      "/opt/bin/jailer",
		FirecrackerBinary: "/opt/bin/firecracker",
		ChrootBaseDir:     "/var/lib/launcher/jails",
		ID:                "inv-123",
	}
	if err := base.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}

	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"empty jailer binary", func(c *Config) { c.JailerBinary = "" }},
		{"empty firecracker binary", func(c *Config) { c.FirecrackerBinary = "" }},
		{"empty chroot base dir", func(c *Config) { c.ChrootBaseDir = "" }},
		{"empty id", func(c *Config) { c.ID = "" }},
		{"id with path traversal", func(c *Config) { c.ID = "../../etc" }},
		{"id with slash", func(c *Config) { c.ID = "a/b" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mod(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error")
			}
		})
	}
}

func TestSocketPath(t *testing.T) {
	short := Config{FirecrackerBinary: "/opt/bin/firecracker", ID: "inv-123", ChrootBaseDir: "/var/lib/launcher/jails"}
	got, err := short.SocketPath("/run/firecracker.socket")
	if err != nil {
		t.Fatalf("SocketPath() = %v, want nil error", err)
	}
	want := filepath.Join(short.JailRoot(), "/run/firecracker.socket")
	if got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}

	long := Config{
		FirecrackerBinary: "/opt/bin/firecracker",
		ID:                "kvm-smoke-agent-stage",
		ChrootBaseDir:     "/home/krzysztof-niemiec/firecracker-poc/run/launcher-chroot",
	}
	if _, err := long.SocketPath("/run/firecracker.socket"); err == nil {
		t.Errorf("SocketPath() = nil error, want an error for a path exceeding %d bytes", maxUnixSocketPathLen)
	}
}

func TestCommand(t *testing.T) {
	c := Config{
		JailerBinary:      "/opt/bin/jailer",
		FirecrackerBinary: "/opt/bin/firecracker",
		ID:                "inv-123",
		ChrootBaseDir:     "/var/lib/launcher/jails",
	}
	cmd := c.Command(context.Background())
	if cmd.Path != "/opt/bin/jailer" {
		t.Errorf("cmd.Path = %q", cmd.Path)
	}
	want := append([]string{"/opt/bin/jailer"}, c.Argv()...)
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}
