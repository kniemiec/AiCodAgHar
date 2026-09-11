// Package config will hold Go structs for .harness/policy.yaml and
// .harness/stages.yaml (Phase 2+). It also hosts the Phase 1 contract-
// validation gate: JSON Schema fixture tests for schemas/stages.schema.json
// and schemas/policy.schema.json, and OpenAPI well-formedness checks for the
// api/openapi/*.yaml specs.
package config
