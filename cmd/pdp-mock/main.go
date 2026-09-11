// pdp-mock is a standalone AuthZEN-shaped HTTP service implementing
// PolicyClient's server side (api/openapi/pdp-authzen.yaml). Per plan
// Decision 7, it computes decisions and resolved_steps by reading the
// seeded config repos' raw files live via Forgejo's contents API, rather
// than a static fixture or the real webhook-triggered bundle builder.
package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	forgejoURL := requireEnv("FORGEJO_URL")
	forgejoToken := requireEnv("FORGEJO_TOKEN")
	ref := os.Getenv("PDP_MOCK_REF")
	if ref == "" {
		ref = "main"
	}

	git, err := gitclient.NewClient(forgejoURL, forgejoToken)
	if err != nil {
		log.Fatalf("pdp-mock: git client: %v", err)
	}

	var baselineRepo *gitclient.RepoRef
	if v := os.Getenv("GLOBAL_BASELINE_REPO"); v != "" {
		owner, name, ok := strings.Cut(v, "/")
		if !ok {
			log.Fatalf("pdp-mock: GLOBAL_BASELINE_REPO must be \"owner/name\", got %q", v)
		}
		baselineRepo = &gitclient.RepoRef{Owner: owner, Name: name}
	}

	d := &decider{git: git, ref: ref, baselineRepo: baselineRepo}
	log.Printf("pdp-mock listening on %s (forgejo=%s ref=%s baseline=%v)", addr, forgejoURL, ref, baselineRepo)
	if err := http.ListenAndServe(addr, logRequests(newServeMux(d))); err != nil {
		log.Fatalf("pdp-mock: %v", err)
	}
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("pdp-mock: required env var %s is not set", name)
	}
	return v
}
