package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config models a kubeconfig-style file with contexts and endpoints.
type Config struct {
	CurrentContext string              `yaml:"currentContext"`
	Contexts       map[string]*Context `yaml:"contexts"`
}

// Context encodes connection details for the control plane API.
type Context struct {
	Server         string `yaml:"server"`
	TimeoutSeconds int    `yaml:"timeoutSeconds"`
	SSHHost        string `yaml:"sshHost"`
}

// ErrContextNotFound indicates the requested context is missing.
var ErrContextNotFound = errors.New("context not found")

// Load decodes the config file. Missing files return (nil, nil).
func Load(path string) (*Config, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, nil
	}
	expanded, err := expandPath(trimmed)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(expanded)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Save writes the config to disk, creating parent directories if needed.
func (c *Config) Save(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config path is required")
	}
	expanded, err := expandPath(path)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(expanded, data, 0o600); err != nil {
		return err
	}
	return nil
}

// Resolve picks a context either by explicit name or the currentContext value.
func (c *Config) Resolve(name string) (*Context, string, error) {
	if c == nil {
		return nil, "", nil
	}
	ctxName := strings.TrimSpace(name)
	if ctxName == "" {
		ctxName = c.CurrentContext
	}
	if ctxName == "" {
		return nil, "", nil
	}
	ctx, ok := c.Contexts[ctxName]
	if !ok {
		return nil, ctxName, fmt.Errorf("%w: %s", ErrContextNotFound, ctxName)
	}
	return ctx, ctxName, nil
}

func expandPath(path string) (string, error) {
	switch {
	case strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	case path == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home, nil
	case filepath.IsAbs(path):
		return path, nil
	default:
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, path), nil
	}
}
