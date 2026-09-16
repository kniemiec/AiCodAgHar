package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

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

type webhookSecretWire struct {
	Secret string `json:"secret"`
}

func newServeMux(p *pool, wp *webhookSecretPool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credentials", handleIssue(p))
	mux.HandleFunc("DELETE /credentials/{id}", handleRevoke(p))
	mux.HandleFunc("GET /webhook-secrets/{owner}/{name}", handleGetWebhookSecret(wp))
	return mux
}

func handleGetWebhookSecret(wp *webhookSecretPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("owner") + "/" + r.PathValue("name")
		secret, ok := wp.get(repo)
		if !ok {
			http.Error(w, "no webhook secret provisioned for "+repo, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(webhookSecretWire{Secret: secret})
	}
}

func handleIssue(p *pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req credentialRequestWire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.BotAccount == "" {
			http.Error(w, "bot_account is required", http.StatusBadRequest)
			return
		}

		id, token, expiresAt, err := p.issue(req.BotAccount, req.Scopes, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			if errors.Is(err, errNoTokenProvisioned) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(credentialWire{ID: id, Token: token, ExpiresAt: expiresAt, Scopes: req.Scopes})
	}
}

func handleRevoke(p *pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !p.revoke(id) {
			http.Error(w, "unknown credential id", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
