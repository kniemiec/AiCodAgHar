package fcapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func newFakeServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir := t.TempDir()
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

func TestPutBootSource(t *testing.T) {
	var gotPath string
	var gotBody BootSourceConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/boot-source", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.PutBootSource(context.Background(), BootSourceConfig{KernelImagePath: "/vmlinux", BootArgs: "console=ttyS0"}); err != nil {
		t.Fatalf("PutBootSource: %v", err)
	}
	if gotPath != "/boot-source" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody.KernelImagePath != "/vmlinux" || gotBody.BootArgs != "console=ttyS0" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestPutDrive(t *testing.T) {
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/drives/rootfs", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.PutDrive(context.Background(), DriveConfig{DriveID: "rootfs", PathOnHost: "/rootfs.ext4", IsRootDevice: true}); err != nil {
		t.Fatalf("PutDrive: %v", err)
	}
	if gotPath != "/drives/rootfs" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestPutMachineConfig(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/machine-config", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.PutMachineConfig(context.Background(), MachineConfig{VCPUCount: 2, MemSizeMib: 1024}); err != nil {
		t.Fatalf("PutMachineConfig: %v", err)
	}
}

func TestPutMMDSConfig(t *testing.T) {
	var gotBody MMDSConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/mmds/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.PutMMDSConfig(context.Background(), MMDSConfig{Version: "V2"}); err != nil {
		t.Fatalf("PutMMDSConfig: %v", err)
	}
	if gotBody.Version != "V2" {
		t.Errorf("body.Version = %q, want V2", gotBody.Version)
	}
}

func TestPutMMDS(t *testing.T) {
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/mmds", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	data := map[string]string{"FORGEJO_TOKEN": "secret-token", "TARGET_REPO": "org/repo"}
	if err := c.PutMMDS(context.Background(), data); err != nil {
		t.Fatalf("PutMMDS: %v", err)
	}
	if gotBody["FORGEJO_TOKEN"] != "secret-token" || gotBody["TARGET_REPO"] != "org/repo" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestPutVsock(t *testing.T) {
	var gotBody VsockConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/vsock", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.PutVsock(context.Background(), VsockConfig{GuestCID: 3, UDSPath: "/vsock.sock"}); err != nil {
		t.Fatalf("PutVsock: %v", err)
	}
	if gotBody.GuestCID != 3 || gotBody.UDSPath != "/vsock.sock" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestPutNetworkInterface(t *testing.T) {
	var gotPath string
	var gotBody NetworkInterfaceConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/network-interfaces/net1", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	cfg := NetworkInterfaceConfig{IfaceID: "net1", GuestMAC: "06:00:AC:10:00:02", HostDevName: "tap0"}
	if err := c.PutNetworkInterface(context.Background(), cfg); err != nil {
		t.Fatalf("PutNetworkInterface: %v", err)
	}
	if gotPath != "/network-interfaces/net1" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody != cfg {
		t.Errorf("body = %+v, want %+v", gotBody, cfg)
	}
}

func TestInstanceStart(t *testing.T) {
	var gotBody action
	mux := http.NewServeMux()
	mux.HandleFunc("/actions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.InstanceStart(context.Background()); err != nil {
		t.Fatalf("InstanceStart: %v", err)
	}
	if gotBody.ActionType != "InstanceStart" {
		t.Errorf("action type = %q", gotBody.ActionType)
	}
}

func TestPutErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/boot-source", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"fault_message":"bad kernel path"}`))
	})
	sock := newFakeServer(t, mux)

	c := NewClient(sock)
	if err := c.PutBootSource(context.Background(), BootSourceConfig{KernelImagePath: "/nope"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "delayed.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- WaitForSocket(ctx, sock, 20*time.Millisecond)
	}()

	time.Sleep(100 * time.Millisecond)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	if err := <-errCh; err != nil {
		t.Fatalf("WaitForSocket: %v", err)
	}
}

func TestWaitForSocketTimeout(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "never.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := WaitForSocket(ctx, sock, 10*time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}
