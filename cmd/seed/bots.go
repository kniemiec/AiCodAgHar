package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

// botAccount is one bot Forgejo account the sample repo's stages.yaml
// references by BotAccount. Password is fixed (not secret — these are
// throwaway local-dev accounts, same posture as deploy/.env.example's
// other local credentials) so re-seeding can always mint a fresh token
// without needing to remember a generated one.
type botAccount struct {
	Username string
	Password string
	Scopes   []string
}

var botAccounts = []botAccount{
	{Username: "review-bot", Password: "seed-bot-password-review", Scopes: []string{"write:repository", "write:issue"}},
	{Username: "ci-bot", Password: "seed-bot-password-ci", Scopes: []string{"write:repository", "write:issue"}},
}

// ensureBotAccountsAndMintTokens creates each bot account if it doesn't
// already exist, mints each one a fresh token, and returns
// username -> token. It does not touch secrets-mock directly (it runs
// outside the compose network by default) — the caller is responsible for
// writing these into deploy/.env's SECRETS_MOCK_POOL_JSON and recreating
// secrets-mock, the same pattern deploy/bootstrap-admin.sh established for
// FORGEJO_ADMIN_TOKEN.
func ensureBotAccountsAndMintTokens(ctx context.Context, git *gitclient.Client, forgejoURL string) (map[string]string, error) {
	tokens := map[string]string{}
	for _, bot := range botAccounts {
		if err := git.EnsureUser(ctx, bot.Username, bot.Password, bot.Username+"@local.invalid"); err != nil {
			return nil, fmt.Errorf("ensure bot account %s: %w", bot.Username, err)
		}
		tokenName := fmt.Sprintf("seed-%d", time.Now().UnixNano())
		token, err := gitclient.MintAccessToken(ctx, forgejoURL, bot.Username, bot.Password, tokenName, bot.Scopes...)
		if err != nil {
			return nil, fmt.Errorf("mint token for %s: %w", bot.Username, err)
		}
		tokens[bot.Username] = token
	}
	return tokens, nil
}

// writeSecretsMockPool updates deploy/.env's SECRETS_MOCK_POOL_JSON in
// place, replacing any prior line — the same append/replace pattern
// deploy/bootstrap-admin.sh uses for FORGEJO_ADMIN_TOKEN.
func writeSecretsMockPool(envPath string, tokens map[string]string) error {
	raw, err := os.ReadFile(envPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", envPath, err)
	}

	var b strings.Builder
	b.WriteString("{")
	first := true
	for _, bot := range botAccounts {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "%q:%q", bot.Username, tokens[bot.Username])
	}
	b.WriteString("}")
	poolLine := "SECRETS_MOCK_POOL_JSON=" + b.String()

	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines)+1)
	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "SECRETS_MOCK_POOL_JSON=") {
			out = append(out, poolLine)
			found = true
			continue
		}
		out = append(out, line)
	}
	if !found {
		out = append(out, poolLine)
	}

	return os.WriteFile(envPath, []byte(strings.Join(out, "\n")), 0o644)
}
