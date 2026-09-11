package main

import (
	"context"
	"fmt"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/config"
)

// decider computes PDP decisions by reading .harness/stages.yaml and
// .harness/policy.yaml live from the target repo (and, per plan Decision 7,
// the platform-owned global-baseline repo's own policy.yaml) via Forgejo's
// contents API, rather than a static fixture or the real webhook-triggered
// bundle builder.
//
// Phase 3 simplification: matching is by task_type and risk_tier only
// (files_touched_patterns conditions are parsed but not evaluated — no
// request field carries a files-touched fact yet), and granted_scopes is a
// pass-through of requested_scopes. Both are documented gaps for a later
// phase, not a silent omission.
type decider struct {
	git          gitclient.GitHostClient
	ref          string
	baselineRepo *gitclient.RepoRef // nil if no global-baseline repo is configured
}

func (d *decider) decide(ctx context.Context, repoRef gitclient.RepoRef, req evaluationRequestWire) (evaluationResponseWire, error) {
	stagesRaw, err := d.git.GetFileContents(ctx, repoRef, ".harness/stages.yaml", d.ref)
	if err != nil {
		return evaluationResponseWire{
			Decision: "deny",
			Reason:   fmt.Sprintf("no .harness/stages.yaml at %s@%s: %v", repoRef.Owner+"/"+repoRef.Name, d.ref, err),
		}, nil
	}
	stages, err := config.ParseStages(stagesRaw)
	if err != nil {
		return evaluationResponseWire{}, fmt.Errorf("parse .harness/stages.yaml: %w", err)
	}

	policy, err := d.loadMergedPolicy(ctx, repoRef)
	if err != nil {
		return evaluationResponseWire{}, err
	}

	riskTier := req.Context.RiskTier
	for _, ov := range policy.RiskTierOverrides {
		if ov.Match.TaskType != "" && ov.Match.TaskType == req.Action.Name {
			riskTier = ov.RiskTier
		}
	}

	var resolved []resolvedStepWire
	for _, s := range stages {
		if s.TaskType != req.Action.Name {
			continue
		}
		if s.Conditions != nil && len(s.Conditions.RiskTier) > 0 && !containsString(s.Conditions.RiskTier, riskTier) {
			continue
		}
		resolved = append(resolved, resolvedStepWire{StepID: s.ID, Type: s.Type, Blocking: s.Blocking, BotAccount: s.BotAccount})
	}

	decision := "allow"
	reason := ""
	for _, ra := range policy.RequireHumanApproval {
		for _, r := range resolved {
			if r.StepID == ra.StepID && (len(ra.RiskTier) == 0 || containsString(ra.RiskTier, riskTier)) {
				decision = "require_human_approval"
				reason = fmt.Sprintf("step %s requires human approval at risk tier %s", ra.StepID, riskTier)
			}
		}
	}

	return evaluationResponseWire{
		Decision:            decision,
		GrantedScopes:       req.Context.RequestedScopes,
		Reason:              reason,
		PolicyBundleVersion: "pdp-mock:live-read@" + d.ref,
		ResolvedSteps:       resolved,
	}, nil
}

// loadMergedPolicy merges the platform-owned global-baseline repo's
// .harness/policy.yaml (if configured) with the target repo's own — a union
// of both narrow-only override sets, since either individually can only
// narrow what's allowed, never grant. Where both specify a risk_tier
// override matching the same task_type, the target repo's is applied after
// the baseline's, so the target's setting wins — the target can narrow
// further, never loosen the baseline. A missing policy.yaml in either repo
// is not an error: no narrow-only overrides there is a valid, fully
// permitted configuration.
func (d *decider) loadMergedPolicy(ctx context.Context, repoRef gitclient.RepoRef) (config.PolicyOverrides, error) {
	var merged config.PolicyOverrides
	if d.baselineRepo != nil {
		raw, err := d.git.GetFileContents(ctx, *d.baselineRepo, ".harness/policy.yaml", d.ref)
		if err == nil {
			baseline, err := config.ParsePolicy(raw)
			if err != nil {
				return config.PolicyOverrides{}, fmt.Errorf("parse global-baseline .harness/policy.yaml: %w", err)
			}
			merged = baseline
		}
	}

	raw, err := d.git.GetFileContents(ctx, repoRef, ".harness/policy.yaml", d.ref)
	if err == nil {
		target, err := config.ParsePolicy(raw)
		if err != nil {
			return config.PolicyOverrides{}, fmt.Errorf("parse .harness/policy.yaml: %w", err)
		}
		merged.RiskTierOverrides = append(merged.RiskTierOverrides, target.RiskTierOverrides...)
		merged.RequireHumanApproval = append(merged.RequireHumanApproval, target.RequireHumanApproval...)
		merged.QuotaRules = append(merged.QuotaRules, target.QuotaRules...)
	}
	return merged, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
