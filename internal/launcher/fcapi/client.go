// Package fcapi is a thin Go client for Firecracker's own HTTP API, reached
// over a Unix domain socket (jailer's --api-sock convention). It exists so
// cmd/container-launcher never needs an external Firecracker Go SDK for the
// handful of endpoints it drives — boot-source, drives, machine-config,
// actions, and MMDS. It has no opinion on what goes into any of these
// bodies (the mmds package layers that on top for MMDS specifically).
package fcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to one Firecracker instance's API socket.
type Client struct {
	httpClient *http.Client
}

// NewClient builds a Client that dials socketPath for every request —
// Firecracker's API has no real host component, it's addressed purely by
// the socket, so the request URL's host is always "localhost" as a
// placeholder.
func NewClient(socketPath string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// BootSourceConfig is the body of PUT /boot-source.
type BootSourceConfig struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

// DriveConfig is the body of PUT /drives/{drive_id}.
type DriveConfig struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// MachineConfig is the body of PUT /machine-config.
type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMib int `json:"mem_size_mib"`
}

// action is the body of PUT /actions.
type action struct {
	ActionType string `json:"action_type"`
}

// MMDSConfig is the body of PUT /mmds/config. NetworkInterfaces lists which
// attached network interfaces' IDs may serve MMDS requests — confirmed
// against the real API by the Firecracker PoC's Step 3
// (docs/plans/firecracker-microvm-launch-poc.md); left unset (omitted) for
// now since no network interface is attached yet (that's the egress step).
type MMDSConfig struct {
	Version           string   `json:"version"`
	NetworkInterfaces []string `json:"network_interfaces,omitempty"`
}

// VsockConfig is the body of PUT /vsock.
type VsockConfig struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// NetworkInterfaceConfig is the body of PUT /network-interfaces/{iface_id}.
// Confirmed against the real API by the Firecracker PoC's Step 2
// (docs/plans/firecracker-microvm-launch-poc.md,
// ~/firecracker-poc/run/step2-console.log): MMDS reachability over this
// interface is a separate opt-in on /mmds/config's own network_interfaces
// list (design §7), not a field here.
type NetworkInterfaceConfig struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name"`
}

// PutBootSource configures the guest kernel and boot arguments. Must be
// called before InstanceStart.
func (c *Client) PutBootSource(ctx context.Context, cfg BootSourceConfig) error {
	return c.put(ctx, "/boot-source", cfg)
}

// PutDrive attaches one block device. Must be called before InstanceStart.
func (c *Client) PutDrive(ctx context.Context, cfg DriveConfig) error {
	return c.put(ctx, "/drives/"+cfg.DriveID, cfg)
}

// PutMachineConfig sets vCPU count and memory size. Must be called before
// InstanceStart.
func (c *Client) PutMachineConfig(ctx context.Context, cfg MachineConfig) error {
	return c.put(ctx, "/machine-config", cfg)
}

// InstanceStart boots the configured guest. No further /boot-source,
// /drives, or /machine-config calls are accepted after this succeeds.
func (c *Client) InstanceStart(ctx context.Context) error {
	return c.put(ctx, "/actions", action{ActionType: "InstanceStart"})
}

// PutMMDSConfig sets the MMDS device's version. The mmds package always
// passes V2 (design §7) — session-token-required mode — never V1, which
// this client still exposes generically since it has no opinion on that
// choice itself.
func (c *Client) PutMMDSConfig(ctx context.Context, cfg MMDSConfig) error {
	return c.put(ctx, "/mmds/config", cfg)
}

// PutMMDS writes the MMDS data document the guest fetches over the
// session-token handshake. data is delivered verbatim, whatever shape it
// marshals to.
func (c *Client) PutMMDS(ctx context.Context, data any) error {
	return c.put(ctx, "/mmds", data)
}

// PutVsock attaches a virtio-vsock device. Must be called before
// InstanceStart. GuestCID must be >= 3 (0/1 are hypervisor-reserved, 2 is
// always the host) — v1 runs one invocation at a time, so a single fixed
// CID is enough; concurrency-safe allocation is follow-up work, the same
// scoping already applied to the jailer uid/gid.
func (c *Client) PutVsock(ctx context.Context, cfg VsockConfig) error {
	return c.put(ctx, "/vsock", cfg)
}

// PutNetworkInterface attaches one virtio-net device backed by a host tap
// device (design §10). Must be called before InstanceStart.
func (c *Client) PutNetworkInterface(ctx context.Context, cfg NetworkInterfaceConfig) error {
	return c.put(ctx, "/network-interfaces/"+cfg.IfaceID, cfg)
}

func (c *Client) put(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("fcapi: marshal %s body: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("fcapi: build request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fcapi: PUT %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fcapi: PUT %s: unexpected status %d: %s", path, resp.StatusCode, respBody)
	}
	return nil
}

// WaitForSocket polls until socketPath accepts a connection or ctx is done.
// jailer creates the jail directory tree asynchronously (it forks, sets up
// the chroot/cgroup, then execs Firecracker), so the API socket doesn't
// exist the instant the jailer process starts.
func WaitForSocket(ctx context.Context, socketPath string, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var lastDialErr error
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		if err == nil {
			conn.Close()
			return nil
		}
		// Kept and reported below rather than discarded: a deadline that
		// expires after every single attempt failed the same way (e.g.
		// ENAMETOOLONG on a too-long socketPath — jailer.Config.SocketPath
		// guards against this ahead of time, but any other per-attempt
		// dial failure deserves the same visibility) should say so, not
		// just "context deadline exceeded".
		lastDialErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("fcapi: waiting for socket %s: %w (last dial attempt: %v)", socketPath, ctx.Err(), lastDialErr)
		case <-ticker.C:
		}
	}
}
