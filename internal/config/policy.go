package config

import "gopkg.in/yaml.v3"

// RiskTierOverrideMatch is a risk_tier_overrides entry's match criteria.
type RiskTierOverrideMatch struct {
	FilesTouchedPatterns []string `yaml:"files_touched_patterns,omitempty"`
	TaskType             string   `yaml:"task_type,omitempty"`
}

// RiskTierOverride narrows the effective risk tier for matching tasks.
type RiskTierOverride struct {
	Match    RiskTierOverrideMatch `yaml:"match"`
	RiskTier string                `yaml:"risk_tier"`
}

// RequireHumanApproval forces a resolved step to require human approval at
// the listed risk tiers (or unconditionally, if RiskTier is empty).
type RequireHumanApproval struct {
	StepID   string   `yaml:"step_id"`
	RiskTier []string `yaml:"risk_tier,omitempty"`
}

// QuotaRule is a narrow-only rate limit on a granted scope. Enforcement is
// not implemented by pdp-mock in Phase 3 — parsed for schema completeness
// only.
type QuotaRule struct {
	Scope         string `yaml:"scope"`
	MaxPerWindow  int    `yaml:"max_per_window"`
	WindowSeconds int    `yaml:"window_seconds"`
}

// PolicyOverrides is the top-level shape of .harness/policy.yaml, matching
// schemas/policy.schema.json's narrow-only override shape.
type PolicyOverrides struct {
	RiskTierOverrides    []RiskTierOverride     `yaml:"risk_tier_overrides,omitempty"`
	RequireHumanApproval []RequireHumanApproval `yaml:"require_human_approval,omitempty"`
	QuotaRules           []QuotaRule            `yaml:"quota_rules,omitempty"`
}

// ParsePolicy parses .harness/policy.yaml content.
func ParsePolicy(data []byte) (PolicyOverrides, error) {
	var p PolicyOverrides
	if err := yaml.Unmarshal(data, &p); err != nil {
		return PolicyOverrides{}, err
	}
	return p, nil
}
