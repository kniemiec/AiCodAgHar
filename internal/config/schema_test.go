package config

import "testing"

func TestStagesSchema(t *testing.T) {
	const schema = "../../schemas/stages.schema.json"

	t.Run("valid fixture passes", func(t *testing.T) {
		if err := ValidateJSON(schema, "../../schemas/fixtures/stages.valid.json"); err != nil {
			t.Fatalf("expected valid fixture to pass, got: %v", err)
		}
	})

	t.Run("invalid fixture fails", func(t *testing.T) {
		if err := ValidateJSON(schema, "../../schemas/fixtures/stages.invalid.json"); err == nil {
			t.Fatal("expected invalid fixture (deterministic-step missing command/working_dir/timeout_seconds) to fail validation")
		}
	})
}

func TestPolicySchema(t *testing.T) {
	const schema = "../../schemas/policy.schema.json"

	t.Run("valid fixture passes", func(t *testing.T) {
		if err := ValidateJSON(schema, "../../schemas/fixtures/policy.valid.json"); err != nil {
			t.Fatalf("expected valid fixture to pass, got: %v", err)
		}
	})

	t.Run("invalid fixture fails", func(t *testing.T) {
		if err := ValidateJSON(schema, "../../schemas/fixtures/policy.invalid.json"); err == nil {
			t.Fatal("expected invalid fixture (unknown grant_extra_scopes field) to fail validation")
		}
	})
}
