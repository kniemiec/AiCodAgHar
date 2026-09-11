// Package secretsclient holds the SecretsClient interface; its production
// implementation over an OpenBao-shaped HTTP API lands in Phase 3.
package secretsclient

import (
	"context"
	"time"
)

// CredentialRequest mirrors api/openapi/secrets.yaml's CredentialRequest.
type CredentialRequest struct {
	InvocationID string
	BotAccount   string
	TargetRepo   string
	Scopes       []string
	TTL          time.Duration
}

// Credential mirrors api/openapi/secrets.yaml's Credential.
type Credential struct {
	ID        string
	Token     string
	ExpiresAt time.Time
	Scopes    []string
}

// SecretsClient is ADR-9's secrets-minting test-seam interface. Its
// production implementation is an HTTP client over an OpenBao-shaped subset
// (api/openapi/secrets.yaml); secrets-mock (Phase 3) implements its server
// side with a static pre-provisioned bot-token pool rather than real
// per-invocation minting/revocation.
type SecretsClient interface {
	IssueCredential(ctx context.Context, req CredentialRequest) (Credential, error)
	RevokeCredential(ctx context.Context, credentialID string) error
}
