package main

import (
	"fmt"
	"sync"
	"time"
)

// pool is the static pre-provisioned bot-token pool (plan Decision 5): it
// hands out one of a fixed set of tokens per bot account rather than
// minting real per-invocation credentials.
type pool struct {
	mu                 sync.Mutex
	tokensByBotAccount map[string]string
	nextID             int
	issued             map[string]issuedCredential
}

type issuedCredential struct {
	BotAccount string
	Token      string
	Scopes     []string
	ExpiresAt  time.Time
	Revoked    bool
}

func newPool(tokensByBotAccount map[string]string) *pool {
	return &pool{
		tokensByBotAccount: tokensByBotAccount,
		issued:             map[string]issuedCredential{},
	}
}

var errNoTokenProvisioned = fmt.Errorf("no token provisioned for bot account")

func (p *pool) issue(botAccount string, scopes []string, ttl time.Duration) (id string, token string, expiresAt time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	token, ok := p.tokensByBotAccount[botAccount]
	if !ok {
		return "", "", time.Time{}, fmt.Errorf("%w: %s", errNoTokenProvisioned, botAccount)
	}

	p.nextID++
	id = fmt.Sprintf("cred-%d", p.nextID)
	expiresAt = time.Now().Add(ttl)
	p.issued[id] = issuedCredential{BotAccount: botAccount, Token: token, Scopes: scopes, ExpiresAt: expiresAt}
	return id, token, expiresAt, nil
}

// revoke reports whether id was a known, previously issued credential.
func (p *pool) revoke(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	cred, ok := p.issued[id]
	if !ok {
		return false
	}
	cred.Revoked = true
	p.issued[id] = cred
	return true
}

// webhookSecretPool is design §4.4/§7's static pre-provisioned per-repo
// webhook-HMAC-secret pool — a separate concept from pool's bot-token
// credentials: a long-lived shared secret read back by repo key, not a
// per-invocation credential with a TTL or a revoke path.
type webhookSecretPool struct {
	secretsByRepo map[string]string // "owner/name" -> secret
}

func newWebhookSecretPool(secretsByRepo map[string]string) *webhookSecretPool {
	return &webhookSecretPool{secretsByRepo: secretsByRepo}
}

func (p *webhookSecretPool) get(repo string) (string, bool) {
	secret, ok := p.secretsByRepo[repo]
	return secret, ok
}
