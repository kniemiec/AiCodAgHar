// seed is a one-shot idempotent CLI that bootstraps Forgejo bot accounts,
// the global-baseline policy repo, and the sample target repo (see
// docs/plans/active/orchestrator-control-plane-mvp.md, Phase 6). It uses
// GitHostClient's production implementation's underlying SDK wrapper
// (*gitclient.Client), extended with provisioning-only methods
// (internal/clients/gitclient/admin.go) that don't belong on the narrow
// GitHostClient runtime interface.
//
// Idempotency strategy: bot accounts are created once and reused
// (EnsureUser) but always minted a fresh token; repos are deleted and
// recreated from scratch on every run, so re-running seed always leaves
// the two config repos in exactly the state this file describes.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

func main() {
	forgejoURL := requireEnv("FORGEJO_URL")
	forgejoToken := requireEnv("FORGEJO_ADMIN_TOKEN")
	owner := requireEnv("FORGEJO_ADMIN_USERNAME")
	envPath := os.Getenv("SEED_ENV_FILE")
	if envPath == "" {
		envPath = "deploy/.env"
	}

	ctx := context.Background()
	git, err := gitclient.NewClient(forgejoURL, forgejoToken)
	if err != nil {
		log.Fatalf("seed: git client: %v", err)
	}

	log.Println("ensuring bot accounts and minting tokens...")
	tokens, err := ensureBotAccountsAndMintTokens(ctx, git, forgejoURL)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	if err := writeSecretsMockPool(envPath, tokens); err != nil {
		log.Fatalf("seed: write secrets-mock pool to %s: %v", envPath, err)
	}
	log.Printf("wrote SECRETS_MOCK_POOL_JSON to %s for %d bot account(s)", envPath, len(tokens))

	log.Println("seeding global-baseline and sample target repos...")
	targetRepo, err := SeedRepos(ctx, git, owner)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}

	fmt.Printf("\nSeed complete.\n")
	fmt.Printf("  global-baseline repo: %s/%s\n", owner, GlobalBaselineRepoName)
	fmt.Printf("  sample target repo:   %s/%s (ready for a task — no branch pre-seeded)\n", owner, targetRepo.Name)
	fmt.Printf("\nRestart secrets-mock (and control-plane's env if GLOBAL_BASELINE_REPO changed) to pick up the new bot tokens:\n")
	fmt.Printf("  docker compose -f deploy/docker-compose.yml up -d secrets-mock\n")
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("seed: required env var %s is not set", name)
	}
	return v
}
