// launcher-mock is a standalone gRPC/mTLS service implementing
// ContainerLauncher's server side (api/proto/launcher/v1/launcher.proto).
// Per plan Decision 6, it is fixture-driven but performs real Forgejo
// side-effects via a small closed set of fixture-driven behaviors
// (cmd/launcher-mock/fixtures.go), proving the credential round-trip and
// real git-visible outcomes without any real sandbox mechanics. It is a
// placeholder for the real ADR-10 jailer/Firecracker launcher behind the
// same gRPC contract.
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
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9443"
	}
	caPath := requireEnv("TLS_CA_CERT")
	certPath := requireEnv("TLS_SERVER_CERT")
	keyPath := requireEnv("TLS_SERVER_KEY")

	creds, err := serverTLSCredentials(caPath, certPath, keyPath)
	if err != nil {
		log.Fatalf("launcher-mock: TLS setup: %v", err)
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("launcher-mock: listen: %v", err)
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds))
	launcherv1.RegisterContainerLauncherServiceServer(grpcServer, newLauncherServer())

	log.Printf("launcher-mock listening on %s (mTLS)", addr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("launcher-mock: serve: %v", err)
	}
}

// serverTLSCredentials requires a client certificate signed by the same dev
// CA — enforcing the control-plane/launcher privilege boundary ADR-11 calls
// out as needing "real authentication" (plan Decision 1).
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

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("launcher-mock: required env var %s is not set", name)
	}
	return v
}
