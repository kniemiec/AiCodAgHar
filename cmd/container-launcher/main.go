// cmd/container-launcher is ContainerLauncherService's production
// implementation (docs/designs/completed/container-launcher-design.md): a
// gRPC+mTLS server, wire-compatible with mocks/launcher-mock, backed by
// jailer-wrapped Firecracker microVMs (ADR-10). See
// docs/components/container-launcher.md for the living, current-state
// developer guide.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
	"github.com/kniemiec/AiCodAgHar/internal/launcher/egress"
)

func main() {
	cfg := loadConfig()

	// Static, one-time host-level egress setup (design §10) — must
	// complete before the first Launch call, so it runs at startup, not
	// per invocation (egress.Setup's own doc comment explains why this is
	// safe to treat as a singleton rather than per-invocation state).
	if err := egress.Setup(egress.DefaultConfig(cfg.JailerUID)); err != nil {
		log.Fatalf("container-launcher: egress setup: %v", err)
	}

	creds, err := serverTLSCredentials(cfg.CACert, cfg.ServerCert, cfg.ServerKey)
	if err != nil {
		log.Fatalf("container-launcher: TLS setup: %v", err)
	}

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatalf("container-launcher: listen: %v", err)
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds))
	launcherv1.RegisterContainerLauncherServiceServer(grpcServer, newLauncherServer(cfg))

	log.Printf("container-launcher listening on %s (mTLS)", cfg.Addr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("container-launcher: serve: %v", err)
	}
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
