package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/kniemiec/AiCodAgHar/internal/health"
)

// readiness builds the launcher's /readyz checks (design §8). The launcher
// has no database; ready means: egress setup finished, the jailer and
// firecracker binaries are executable, the image root is readable and
// /dev/kvm opens. egressReady is flipped by main once egress.Setup returns.
func readiness(cfg config, egressReady *atomic.Bool) []health.Check {
	return []health.Check{
		{Name: "egress", Fn: func(context.Context) error {
			if !egressReady.Load() {
				return fmt.Errorf("egress setup not complete")
			}
			return nil
		}},
		{Name: "jailer_binary", Fn: func(context.Context) error { return executable(cfg.JailerBinary) }},
		{Name: "firecracker_binary", Fn: func(context.Context) error { return executable(cfg.FirecrackerBinary) }},
		{Name: "image_root", Fn: func(context.Context) error {
			_, err := os.ReadDir(cfg.ImageRoot)
			return err
		}},
		{Name: "dev_kvm", Fn: func(context.Context) error {
			f, err := os.OpenFile(kvmDevice, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}},
	}
}

const kvmDevice = "/dev/kvm"

func executable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", path)
	}
	return syscall.Access(path, 0x1) // X_OK for the effective caller
}

// healthServer serves /healthz and /readyz over plain HTTP: the gRPC port is
// mTLS-only, so probes (and a local Collector) need their own listener.
func healthServer(cfg config, egressReady *atomic.Bool) *http.Server {
	mux := http.NewServeMux()
	health.Register(mux, readiness(cfg, egressReady)...)
	return &http.Server{Addr: cfg.HealthAddr, Handler: mux}
}
