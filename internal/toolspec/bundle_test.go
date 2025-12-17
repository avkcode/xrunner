package toolspec

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestBundleValidate(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	bundle := Bundle{
		SchemaVersion: "2025-12-15",
		BundleVersion: "v2025-12-15",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Tools: []ToolDefinition{
			{
				Type: "function",
				Function: FunctionSpec{
					Name:        "do_work",
					Description: "Invoke the worker",
					Parameters:  schema,
				},
			},
		},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBundleValidateDuplicateNames(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	bundle := Bundle{
		SchemaVersion: "2025-12-15",
		BundleVersion: "v2025-12-15",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Tools: []ToolDefinition{
			{
				Type:     "function",
				Function: FunctionSpec{Name: "dup", Description: "one", Parameters: schema},
			},
			{
				Type:     "function",
				Function: FunctionSpec{Name: "dup", Description: "two", Parameters: schema},
			},
		},
	}
	if err := bundle.Validate(); err == nil {
		t.Fatalf("expected duplicate error")
	}
}

func TestLoadBundleFromEnv(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	bundle := Bundle{
		SchemaVersion: "2025-12-15",
		BundleVersion: "v2025-12-15",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Tools: []ToolDefinition{
			{Type: "function", Function: FunctionSpec{Name: "ping", Description: "Ping remote worker", Parameters: schema}},
		},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tmp, err := os.CreateTemp(t.TempDir(), "bundle-*.json")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := tmp.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	tmp.Close()
	envKey := "TEST_TOOL_SPEC"
	t.Setenv(envKey, tmp.Name())
	loaded, err := LoadBundleFromEnv(envKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.BundleVersion != bundle.BundleVersion {
		t.Fatalf("expected version %s, got %s", bundle.BundleVersion, loaded.BundleVersion)
	}
}
