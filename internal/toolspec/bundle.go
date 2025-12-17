package toolspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Bundle struct {
	SchemaVersion string           `json:"schema_version"`
	BundleVersion string           `json:"bundle_version"`
	GeneratedAt   string           `json:"generated_at"`
	Tools         []ToolDefinition `json:"tools"`
}

type ToolDefinition struct {
	Type     string       `json:"type"`
	Strict   *bool        `json:"strict,omitempty"`
	Function FunctionSpec `json:"function"`
}

type FunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

var bundleVersionRe = regexp.MustCompile(`^v[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
var schemaVersionRe = regexp.MustCompile(`^20[0-9]{2}-[0-9]{2}-[0-9]{2}$`)
var functionNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func LoadBundle(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, err
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func LoadBundleFromEnv(envKey string) (*Bundle, error) {
	if envKey == "" {
		envKey = "AGENT_TOOL_SPEC"
	}
	path := os.Getenv(envKey)
	if path == "" {
		return nil, fmt.Errorf("%s not set", envKey)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return LoadBundle(abs)
}

func (b *Bundle) Validate() error {
	if b.SchemaVersion == "" {
		return errors.New("schema_version is required")
	}
	if !schemaVersionRe.MatchString(b.SchemaVersion) {
		return fmt.Errorf("schema_version must match YYYY-MM-DD, got %s", b.SchemaVersion)
	}
	if b.BundleVersion == "" {
		return errors.New("bundle_version is required")
	}
	if !bundleVersionRe.MatchString(b.BundleVersion) {
		return fmt.Errorf("bundle_version must match vYYYY-MM-DD, got %s", b.BundleVersion)
	}
	if b.GeneratedAt == "" {
		return errors.New("generated_at is required")
	}
	if _, err := time.Parse(time.RFC3339, b.GeneratedAt); err != nil {
		return fmt.Errorf("generated_at must be RFC3339: %w", err)
	}
	if len(b.Tools) == 0 {
		return errors.New("tools must contain at least one entry")
	}
	seen := make(map[string]struct{})
	for idx, tool := range b.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("tool[%d]: %w", idx, err)
		}
		name := tool.Function.Name
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate tool name %s", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (t *ToolDefinition) Validate() error {
	if t.Type != "function" {
		return fmt.Errorf("type must be function, got %s", t.Type)
	}
	if err := t.Function.Validate(); err != nil {
		return err
	}
	if len(t.Function.Parameters) == 0 {
		return errors.New("function.parameters is required")
	}
	trimmed := strings.TrimSpace(string(t.Function.Parameters))
	if !strings.HasPrefix(trimmed, "{") {
		return errors.New("function.parameters must be a JSON object")
	}
	return nil
}

func (f *FunctionSpec) Validate() error {
	if f.Name == "" {
		return errors.New("function.name is required")
	}
	if !functionNameRe.MatchString(f.Name) {
		return fmt.Errorf("function.name must match [A-Za-z0-9_]{1,64}, got %s", f.Name)
	}
	if len(strings.TrimSpace(f.Description)) < 8 {
		return errors.New("function.description must be at least 8 characters")
	}
	return nil
}
