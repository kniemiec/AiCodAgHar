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
	TaskType               string // new-task (only live value) | review | security-triage
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
//
// json tags matter here: this type is also what internal/control-plane/coreloop marshals
// verbatim into the audit trail's resolved_steps column (Phase 5), so its
// wire shape should match api/openapi/pdp-authzen.yaml's snake_case
// ResolvedStep schema, not Go's default capitalized field names.
type ResolvedStep struct {
	StepID     string `json:"step_id"`
	Type       string `json:"type"` // agent-stage | deterministic-step
	Blocking   bool   `json:"blocking"`
	BotAccount string `json:"bot_account"`

	// Phase is the follow-up design's addition
	// (docs/designs/completed/full-pipeline-async-approval-design.md §4.1):
	// generate | review | approve | publish. Empty means "review", the
	// default for steps resolved before this field existed.
	Phase string `json:"phase,omitempty"`
}

// The fixed, hardcoded phase set and dispatch order (design §4.1).
const (
	PhaseGenerate = "generate"
	PhaseReview   = "review"
	PhaseApprove  = "approve"
	PhasePublish  = "publish"
)

// PolicyDecision mirrors api/openapi/pdp-authzen.yaml's EvaluationResponse.
type PolicyDecision struct {
	// Decision is allow | deny (design §4.4 dropped require_human_approval
	// — a human-approval gate is now a resolved step with Phase ==
	// PhaseApprove on an ordinary allow).
	Decision            string
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
