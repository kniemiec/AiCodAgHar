package secretsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is SecretsClient's production implementation: an HTTP client over
// the OpenBao-shaped subset in api/openapi/secrets.yaml.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient builds a Client against a secrets service at baseURL (e.g.
// secrets-mock, or the real OpenBao-fronting service later).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: http.DefaultClient,
	}
}

type credentialRequestWire struct {
	InvocationID string   `json:"invocation_id"`
	BotAccount   string   `json:"bot_account"`
	TargetRepo   string   `json:"target_repo"`
	Scopes       []string `json:"scopes"`
	TTLSeconds   int      `json:"ttl_seconds"`
}

type credentialWire struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Scopes    []string  `json:"scopes"`
}

func (c *Client) IssueCredential(ctx context.Context, req CredentialRequest) (Credential, error) {
	body, err := json.Marshal(credentialRequestWire{
		InvocationID: req.InvocationID,
		BotAccount:   req.BotAccount,
		TargetRepo:   req.TargetRepo,
		Scopes:       req.Scopes,
		TTLSeconds:   int(req.TTL.Seconds()),
	})
	if err != nil {
		return Credential{}, fmt.Errorf("secretsclient: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/credentials", bytes.NewReader(body))
	if err != nil {
		return Credential{}, fmt.Errorf("secretsclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Credential{}, fmt.Errorf("secretsclient: issue credential: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return Credential{}, fmt.Errorf("secretsclient: issue credential: unexpected status %d: %s", resp.StatusCode, readBody(resp.Body))
	}

	var wire credentialWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return Credential{}, fmt.Errorf("secretsclient: decode response: %w", err)
	}
	return Credential{ID: wire.ID, Token: wire.Token, ExpiresAt: wire.ExpiresAt, Scopes: wire.Scopes}, nil
}

func (c *Client) RevokeCredential(ctx context.Context, credentialID string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/credentials/"+url.PathEscape(credentialID), nil)
	if err != nil {
		return fmt.Errorf("secretsclient: build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("secretsclient: revoke credential: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secretsclient: revoke credential: unexpected status %d: %s", resp.StatusCode, readBody(resp.Body))
	}
	return nil
}

func readBody(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}
