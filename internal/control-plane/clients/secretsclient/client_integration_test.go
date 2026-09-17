//go:build integration

package secretsclient_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/secretsclient"
)

func TestClient_IssueAndRevokeCredential(t *testing.T) {
	baseURL := os.Getenv("SECRETS_MOCK_URL")
	if baseURL == "" {
		t.Skip("SECRETS_MOCK_URL not set; source deploy/.env first")
	}

	client := secretsclient.NewClient(baseURL)
	ctx := context.Background()

	cred, err := client.IssueCredential(ctx, secretsclient.CredentialRequest{
		InvocationID: "inv-1",
		BotAccount:   "review-bot",
		TargetRepo:   "owner/repo",
		Scopes:       []string{"write:repository"},
		TTL:          time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if cred.Token == "" {
		t.Fatal("expected a non-empty token from the pre-provisioned pool")
	}
	if cred.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expected ExpiresAt in the future, got %v", cred.ExpiresAt)
	}

	if err := client.RevokeCredential(ctx, cred.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
}

func TestClient_IssueCredential_UnknownBotAccountFails(t *testing.T) {
	baseURL := os.Getenv("SECRETS_MOCK_URL")
	if baseURL == "" {
		t.Skip("SECRETS_MOCK_URL not set; source deploy/.env first")
	}

	client := secretsclient.NewClient(baseURL)
	_, err := client.IssueCredential(context.Background(), secretsclient.CredentialRequest{
		InvocationID: "inv-2",
		BotAccount:   "no-such-bot",
		TargetRepo:   "owner/repo",
		Scopes:       []string{"write:repository"},
		TTL:          time.Hour,
	})
	if err == nil {
		t.Fatal("expected an error for a bot account with no provisioned token")
	}
}
