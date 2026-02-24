package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProviderBaseURLOverrideAppliesToBuiltInModel(t *testing.T) {
	tmp := t.TempDir()
	modelsPath := filepath.Join(tmp, "models.json")
	if err := os.WriteFile(modelsPath, []byte(`{
		"providers": {
			"openai": {
				"baseUrl": "http://localhost:18181/v1"
			}
		}
	}`), 0o644); err != nil {
		t.Fatalf("write models.json: %v", err)
	}

	reg := NewModelRegistry(nil, modelsPath)
	model, err := reg.ResolveModel("openai", "gpt-5.1-codex")
	if err != nil {
		t.Fatalf("ResolveModel failed: %v", err)
	}
	if model.BaseURL != "http://localhost:18181/v1" {
		t.Fatalf("model base URL = %q, want provider override", model.BaseURL)
	}
}
