package config

import (
	"os"
	"testing"
)

func TestParseStages_ValidFixture(t *testing.T) {
	data, err := os.ReadFile("../../schemas/fixtures/stages.valid.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	stages, err := ParseStages(data)
	if err != nil {
		t.Fatalf("ParseStages: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("expected 2 stage entries, got %d", len(stages))
	}
	if stages[0].Type != "agent-stage" || stages[1].Type != "deterministic-step" {
		t.Fatalf("unexpected stage types: %+v", stages)
	}
}

func TestParsePolicy_ValidFixture(t *testing.T) {
	data, err := os.ReadFile("../../schemas/fixtures/policy.valid.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	policy, err := ParsePolicy(data)
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if len(policy.RiskTierOverrides) != 1 || len(policy.RequireHumanApproval) != 1 || len(policy.QuotaRules) != 1 {
		t.Fatalf("unexpected parse result: %+v", policy)
	}
}
