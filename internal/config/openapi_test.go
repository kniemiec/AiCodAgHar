package config

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPISpecsAreWellFormed(t *testing.T) {
	specs := []string{
		"../../api/openapi/pdp-authzen.yaml",
		"../../api/openapi/secrets.yaml",
	}

	for _, path := range specs {
		t.Run(path, func(t *testing.T) {
			loader := openapi3.NewLoader()
			doc, err := loader.LoadFromFile(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if err := doc.Validate(context.Background()); err != nil {
				t.Fatalf("%s failed OpenAPI validation: %v", path, err)
			}
		})
	}
}
