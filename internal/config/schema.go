package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ValidateJSON validates the JSON document at dataPath against the JSON Schema
// file at schemaPath, returning a descriptive error on any schema violation.
func ValidateJSON(schemaPath, dataPath string) error {
	schema, err := jsonschema.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("compile schema %s: %w", schemaPath, err)
	}

	raw, err := os.ReadFile(dataPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", dataPath, err)
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", dataPath, err)
	}

	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("%s does not satisfy %s: %w", dataPath, schemaPath, err)
	}
	return nil
}
