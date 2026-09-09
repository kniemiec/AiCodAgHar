package main

import (
	"context"
	"fmt"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/config"
)

// decider computes PDP decisions by reading .harness/stages.yaml and
// .harness/policy.yaml live from the target repo via Forgejo's contents API
// (plan Decision 7), rather than a static fixture or the real
// webhook-triggered bundle builder.
//
// Phase 3 simplification: matching is by task_type and risk_tier only
// (files_touched_patterns conditions are parsed but not evaluated — no
// request field carries a files-touched fact yet), and granted_scopes is a
// pass-through of requested_scopes. Both are documented gaps for a later
// phase, not a silent omission.
type decider struct {
	git gitclient.GitHostClient
	ref string
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

	var policy config.PolicyOverrides
	policyRaw, err := d.git.GetFileContents(ctx, repoRef, ".harness/policy.yaml", d.ref)
	if err == nil {
		policy, err = config.ParsePolicy(policyRaw)
		if err != nil {
			return evaluationResponseWire{}, fmt.Errorf("parse .harness/policy.yaml: %w", err)
		}
	}
	// A missing .harness/policy.yaml is not an error: it is a repo with no
	// narrow-only overrides, which is a valid, fully-permitted configuration.

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

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
