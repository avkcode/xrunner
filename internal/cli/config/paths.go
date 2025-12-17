package config

import (
	"os"
	"path/filepath"
)

func DefaultConfigDir() string {
	if v := os.Getenv("XRUNNER_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".xrunner")
}

func DefaultConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "config")
}
