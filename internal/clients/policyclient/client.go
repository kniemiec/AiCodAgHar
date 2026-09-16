package policyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client is PolicyClient's production implementation: an AuthZEN
// Authorization API HTTP client over api/openapi/pdp-authzen.yaml.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient builds a Client against a PDP at baseURL (e.g. pdp-mock, or the
// real OPA-fronting AuthZEN service later).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: http.DefaultClient,
	}
}

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
	Phase      string `json:"phase,omitempty"`
}

type evaluationResponseWire struct {
	Decision            string             `json:"decision"`
	GrantedScopes       []string           `json:"granted_scopes,omitempty"`
	Reason              string             `json:"reason,omitempty"`
	PolicyBundleVersion string             `json:"policy_bundle_version,omitempty"`
	ResolvedSteps       []resolvedStepWire `json:"resolved_steps,omitempty"`
}

func (c *Client) Evaluate(ctx context.Context, req PolicyRequest) (PolicyDecision, error) {
	wire := evaluationRequestWire{
		Subject:  subjectWire{Type: "bot_account", ID: req.BotAccount},
		Resource: resourceWire{Type: "repository", ID: req.TargetRepo},
		Action:   actionWire{Name: req.TaskType},
		Context: contextWire{
			InvocationID:           req.InvocationID,
			TaskID:                 req.TaskID,
			OrchestratorInstanceID: req.OrchestratorInstanceID,
			RiskTier:               req.RiskTier,
			RequestedScopes:        req.RequestedScopes,
			Facts:                  req.Facts,
		},
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return PolicyDecision{}, fmt.Errorf("policyclient: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/access/v1/evaluation", bytes.NewReader(body))
	if err != nil {
		return PolicyDecision{}, fmt.Errorf("policyclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return PolicyDecision{}, fmt.Errorf("policyclient: evaluate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return PolicyDecision{}, fmt.Errorf("policyclient: evaluate: unexpected status %d: %s", resp.StatusCode, readBody(resp.Body))
	}

	var out evaluationResponseWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PolicyDecision{}, fmt.Errorf("policyclient: decode response: %w", err)
	}

	resolved := make([]ResolvedStep, len(out.ResolvedSteps))
	for i, s := range out.ResolvedSteps {
		resolved[i] = ResolvedStep{StepID: s.StepID, Type: s.Type, Blocking: s.Blocking, BotAccount: s.BotAccount, Phase: s.Phase}
	}

	return PolicyDecision{
		Decision:            out.Decision,
		GrantedScopes:       out.GrantedScopes,
		Reason:              out.Reason,
		PolicyBundleVersion: out.PolicyBundleVersion,
		ResolvedSteps:       resolved,
	}, nil
}

func readBody(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}
