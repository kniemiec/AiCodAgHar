// secrets-mock is a standalone HTTP service implementing SecretsClient's
// server side (api/openapi/secrets.yaml) with a static pre-provisioned
// bot-token pool (see docs/plans/active/orchestrator-control-plane-mvp.md,
// Decision 5) rather than real per-invocation minting/revocation.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func defaultTokenPool() map[string]string {
	return map[string]string{
		"review-bot": "static-dev-token-review-bot",
		"ci-bot":     "static-dev-token-ci-bot",
	}
}

func loadTokenPool() map[string]string {
	raw := os.Getenv("SECRETS_MOCK_POOL_JSON")
	if raw == "" {
		return defaultTokenPool()
	}
	var pool map[string]string
	if err := json.Unmarshal([]byte(raw), &pool); err != nil {
		log.Fatalf("secrets-mock: invalid SECRETS_MOCK_POOL_JSON: %v", err)
	}
	return pool
}

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	p := newPool(loadTokenPool())
	log.Printf("secrets-mock listening on %s", addr)
	if err := http.ListenAndServe(addr, logRequests(newServeMux(p))); err != nil {
		log.Fatalf("secrets-mock: %v", err)
	}
}
