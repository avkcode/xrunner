package main

import (
	"os"
	"path/filepath"
)

func defaultConfigDir() string {
	if v := os.Getenv("XRUNNER_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".xrunner")
}

func defaultConfigPath() string {
	return filepath.Join(defaultConfigDir(), "config")
}
