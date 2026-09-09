package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
)

type subjectWire struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type resourceWire struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type actionWire struct {
	Name string `json:"name"`
}

type contextWire struct {
	InvocationID           string         `json:"invocation_id"`
	TaskID                 string         `json:"task_id"`
	OrchestratorInstanceID string         `json:"orchestrator_instance_id"`
	RiskTier               string         `json:"risk_tier"`
	RequestedScopes        []string       `json:"requested_scopes,omitempty"`
	Facts                  map[string]any `json:"facts,omitempty"`
}

type evaluationRequestWire struct {
	Subject  subjectWire  `json:"subject"`
	Resource resourceWire `json:"resource"`
	Action   actionWire   `json:"action"`
	Context  contextWire  `json:"context"`
}

type resolvedStepWire struct {
	StepID     string `json:"step_id"`
	Type       string `json:"type"`
	Blocking   bool   `json:"blocking"`
	BotAccount string `json:"bot_account"`
}

type evaluationResponseWire struct {
	Decision            string             `json:"decision"`
	GrantedScopes       []string           `json:"granted_scopes,omitempty"`
	Reason              string             `json:"reason,omitempty"`
	PolicyBundleVersion string             `json:"policy_bundle_version,omitempty"`
	ResolvedSteps       []resolvedStepWire `json:"resolved_steps,omitempty"`
}

func newServeMux(d *decider) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /access/v1/evaluation", handleEvaluate(d))
	return mux
}

func handleEvaluate(d *decider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req evaluationRequestWire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		owner, name, ok := strings.Cut(req.Resource.ID, "/")
		if !ok {
			http.Error(w, "resource.id must be \"owner/name\"", http.StatusBadRequest)
			return
		}

		resp, err := d.decide(r.Context(), gitclient.RepoRef{Owner: owner, Name: name}, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
