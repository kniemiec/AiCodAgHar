// cmd/container-launcher is ContainerLauncherService's production
// implementation (docs/designs/completed/container-launcher-design.md): a
// gRPC+mTLS server, wire-compatible with mocks/launcher-mock, backed by
// jailer-wrapped Firecracker microVMs (ADR-10). See
// docs/components/container-launcher.md for the living, current-state
// developer guide.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/buildinfo"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/egress"
	"github.com/kniemiec/AiCodAgHar/internal/logging"
	"github.com/kniemiec/AiCodAgHar/internal/telemetry"
)

func main() {
	// Telemetry first, so the OTLP log sink (when enabled) sees startup too.
	tel, err := telemetry.Setup(context.Background(), "container-launcher")
	if err != nil {
		logging.Setup("container-launcher")
		log.Fatalf("container-launcher: telemetry: %v", err)
	}
	logging.Setup("container-launcher", tel.LogHandler)
	tel.RegisterBuildInfo()
	cfg := loadConfig()
	slog.Info("container-launcher starting",
		"version", buildinfo.Version, "commit", buildinfo.Commit, "health_addr", cfg.HealthAddr,
		"addr", cfg.Addr, "chroot_base_dir", cfg.ChrootBaseDir, "image_root", cfg.ImageRoot,
		"jailer_binary", cfg.JailerBinary, "firecracker_binary", cfg.FirecrackerBinary,
		"jailer_uid", cfg.JailerUID, "jailer_gid", cfg.JailerGID,
		"default_timeout_seconds", cfg.DefaultTimeoutSeconds,
		"llm_model", cfg.LLMModel, "llm_api_key_set", cfg.LLMAPIKey != "",
		"otlp_export", tel.LogHandler != nil)

	// Static, one-time host-level egress setup (design §10) — must
	// complete before the first Launch call, so it runs at startup, not
	// per invocation (egress.Setup's own doc comment explains why this is
	// safe to treat as a singleton rather than per-invocation state).
	// The health listener starts first so /readyz can report "egress not
	// complete" while setup runs, instead of the probe being refused.
	var egressReady atomic.Bool
	healthSrv := healthServer(cfg, &egressReady)
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("container-launcher: health listener: %v", err)
		}
	}()

	egressCfg := egress.DefaultConfig(cfg.JailerUID)
	if err := egress.Setup(egressCfg); err != nil {
		log.Fatalf("container-launcher: egress setup: %v", err)
	}
	egressReady.Store(true)
	slog.Info("egress ready", "tap_device", egressCfg.TapDevice, "tap_cidr", egressCfg.TapCIDR,
		"guest_subnet", egressCfg.GuestSubnet, "proxy_service", egressCfg.ProxyServiceName, "nft_table", egressCfg.NftTable, "proxy_url", egressCfg.ProxyURL())

	creds, err := serverTLSCredentials(cfg.CACert, cfg.ServerCert, cfg.ServerKey)
	if err != nil {
		log.Fatalf("container-launcher: TLS setup: %v", err)
	}

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatalf("container-launcher: listen: %v", err)
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds), grpc.UnaryInterceptor(logUnaryRPC))
	srv := newLauncherServer(cfg)
	srv.metrics = newLauncherMetrics(tel.Meter(), srv)
	launcherv1.RegisterContainerLauncherServiceServer(grpcServer, srv)

	// On SIGINT/SIGTERM stop serving (in-flight invocations are cancelled,
	// which tears their VMs down) and flush telemetry before exiting.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		slog.Info("container-launcher shutting down")
		grpcServer.Stop()
	}()

	slog.Info("container-launcher listening (mTLS)", "addr", cfg.Addr)
	serveErr := grpcServer.Serve(lis)
	flushCtx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	if err := tel.Shutdown(flushCtx); err != nil {
		slog.Warn("container-launcher: telemetry shutdown", "error", err)
	}
	if serveErr != nil && ctx.Err() == nil {
		log.Fatalf("container-launcher: serve: %v", serveErr)
	}
}

// telemetryFlushTimeout bounds the final telemetry flush on exit.
const telemetryFlushTimeout = 5 * time.Second

// logUnaryRPC logs every RPC with its outcome code. Requests rejected
// before the handler logs anything (e.g. InvalidArgument) still show up.
func logUnaryRPC(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	var invocationID string
	if r, ok := req.(interface{ GetInvocationId() string }); ok {
		invocationID = r.GetInvocationId()
	}
	resp, err := handler(ctx, req)
	st := status.Convert(err)
	attrs := []any{"method", info.FullMethod, "invocation_id", invocationID, "code", st.Code().String(), "duration", time.Since(start)}
	switch st.Code() {
	case codes.OK:
		slog.InfoContext(ctx, "rpc", attrs...)
	case codes.InvalidArgument, codes.NotFound, codes.Canceled:
		slog.WarnContext(ctx, "rpc rejected", append(attrs, "message", st.Message())...)
	default:
		slog.ErrorContext(ctx, "rpc failed", append(attrs, "message", st.Message())...)
	}
	return resp, err
}

// serverTLSCredentials requires a client certificate signed by the same dev
// CA as launcher-mock — the mTLS contract is unchanged (design §2).
func serverTLSCredentials(caPath, certPath, keyPath string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}), nil
}
