package launcherclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	launcherv1 "github.com/kniemiec/AiCodAgHar/api/gen/go/launcher/v1"
)

// Client is ContainerLauncher's production implementation: a gRPC + mTLS
// client over api/proto/launcher/v1/launcher.proto.
type Client struct {
	conn *grpc.ClientConn
	rpc  launcherv1.ContainerLauncherServiceClient
}

// TLSConfig names the dev-CA-issued client identity used to dial the
// launcher (launcher-mock in this plan; the real ADR-10 launcher later).
type TLSConfig struct {
	CACertPath     string
	ClientCertPath string
	ClientKeyPath  string
}

// NewClient dials a ContainerLauncher server at addr (host:port) over mTLS.
func NewClient(addr string, tlsCfg TLSConfig) (*Client, error) {
	creds, err := clientTLSCredentials(tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("launcherclient: TLS setup: %w", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("launcherclient: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, rpc: launcherv1.NewContainerLauncherServiceClient(conn)}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

func clientTLSCredentials(cfg TLSConfig) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate at %s", cfg.CACertPath)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}), nil
}

func (c *Client) Launch(ctx context.Context, req LaunchRequest) (RunResult, error) {
	resp, err := c.rpc.Launch(ctx, &launcherv1.LaunchRequest{
		InvocationId:   req.InvocationID,
		TaskId:         req.TaskID,
		StepType:       req.StepType,
		StepId:         req.StepID,
		PayloadRef:     req.PayloadRef,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
		GrantedScopes:  req.GrantedScopes,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("launcherclient: launch %s: %w", req.InvocationID, err)
	}

	result := RunResult{
		InvocationID: resp.InvocationId,
		ExitCode:     resp.ExitCode,
		Status:       resp.Status,
	}
	if resp.StartedAt != nil {
		result.StartedAt = resp.StartedAt.AsTime()
	}
	if resp.FinishedAt != nil {
		result.FinishedAt = resp.FinishedAt.AsTime()
	}
	return result, nil
}

func (c *Client) Kill(ctx context.Context, invocationID string) error {
	_, err := c.rpc.Kill(ctx, &launcherv1.KillRequest{InvocationId: invocationID})
	if err != nil {
		return fmt.Errorf("launcherclient: kill %s: %w", invocationID, err)
	}
	return nil
}

func (c *Client) ExtractLogs(ctx context.Context, invocationID string) (LogBundle, error) {
	resp, err := c.rpc.ExtractLogs(ctx, &launcherv1.ExtractLogsRequest{InvocationId: invocationID})
	if err != nil {
		return LogBundle{}, fmt.Errorf("launcherclient: extract logs %s: %w", invocationID, err)
	}
	return LogBundle{
		Stdout:               resp.Stdout,
		Stderr:               resp.Stderr,
		StructuredResultJSON: resp.StructuredResultJson,
	}, nil
}
