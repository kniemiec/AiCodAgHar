// Package policyclient holds the PolicyClient interface; its production
// implementation over the AuthZEN Authorization API lands in Phase 3.
package policyclient

import "context"

// PolicyRequest mirrors api/openapi/pdp-authzen.yaml's EvaluationRequest.
type PolicyRequest struct {
	InvocationID           string
	TaskID                 string
	TriggeringPrincipal    string
	OrchestratorInstanceID string
	BotAccount             string
	TargetRepo             string
	TaskType               string // new-task | pr-review-round | review | security-triage
	RiskTier               string
	RequestedScopes        []string
	Facts                  map[string]any // round number, files touched, etc.
}

// ResolvedStep is ADR-11's PDP-response extension: per-task step
// applicability, resolved by the same call that produces the allow/deny
// decision.
//
// BotAccount is a Phase 4 addition to the contract fixed in Phase 1: the
// executor needs to know which bot account to mint a step's credential as,
// and the base AuthZEN response has nowhere else to carry it. See
// docs/plans/active/orchestrator-control-plane-mvp.md's Architect
// Perspective note on contract mismatches surfacing during implementation.
type ResolvedStep struct {
	StepID     string
	Type       string // agent-stage | deterministic-step
	Blocking   bool
	BotAccount string
}

// PolicyDecision mirrors api/openapi/pdp-authzen.yaml's EvaluationResponse.
type PolicyDecision struct {
	Decision            string // allow | deny | require_human_approval
	GrantedScopes       []string
	Reason              string
	PolicyBundleVersion string
	ResolvedSteps       []ResolvedStep
}

// PolicyClient is ADR-3/ADR-9's fail-closed PDP test-seam interface. Its
// production implementation is an AuthZEN HTTP client over
// api/openapi/pdp-authzen.yaml; pdp-mock (Phase 3) implements its server
// side by reading the two seeded config repos' raw files live.
type PolicyClient interface {
	Evaluate(ctx context.Context, req PolicyRequest) (PolicyDecision, error)
}
