package fakes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/secretsclient"
)

// FakeSecretsClient is an in-memory SecretsClient. It mints a deterministic
// token per call and tracks revocations, without any real static pool.
type FakeSecretsClient struct {
	mu sync.Mutex

	nextID  int
	issued  map[string]secretsclient.Credential
	revoked map[string]bool

	// WebhookSecrets preloads GetWebhookSecret's static pool, keyed by
	// "owner/name". A missing entry is an error, matching secrets-mock's
	// real fail-closed behavior for an unprovisioned repo.
	WebhookSecrets map[string]string
}

func (f *FakeSecretsClient) IssueCredential(ctx context.Context, req secretsclient.CredentialRequest) (secretsclient.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issued == nil {
		f.issued = map[string]secretsclient.Credential{}
	}
	f.nextID++
	cred := secretsclient.Credential{
		ID:        fmt.Sprintf("cred-%d", f.nextID),
		Token:     fmt.Sprintf("fake-token-%d", f.nextID),
		ExpiresAt: time.Now().Add(req.TTL),
		Scopes:    req.Scopes,
	}
	f.issued[cred.ID] = cred
	return cred, nil
}

func (f *FakeSecretsClient) RevokeCredential(ctx context.Context, credentialID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked == nil {
		f.revoked = map[string]bool{}
	}
	f.revoked[credentialID] = true
	return nil
}

// IsRevoked reports whether RevokeCredential has been called for credentialID.
func (f *FakeSecretsClient) IsRevoked(credentialID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked[credentialID]
}

func (f *FakeSecretsClient) GetWebhookSecret(ctx context.Context, targetRepo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	secret, ok := f.WebhookSecrets[targetRepo]
	if !ok {
		return "", fmt.Errorf("fake secrets client: no webhook secret provisioned for %s", targetRepo)
	}
	return secret, nil
}
