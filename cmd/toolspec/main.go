package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antonkrylov/xrunner/internal/toolspec"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "bundle":
		err = runBundle(args)
	case "print-env":
		err = runPrintEnv(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: toolspec <bundle|print-env> [flags]\n")
}

func runBundle(args []string) error {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	defsDir := fs.String("defs", "tools/defs", "directory of *.tool.json files")
	out := fs.String("out", "", "output bundle path")
	version := fs.String("version", "", "bundle version, e.g. v2025-12-15")
	schemaVersion := fs.String("schema-version", time.Now().UTC().Format("2006-01-02"), "schema version tag")
	pretty := fs.Bool("pretty", true, "pretty-print JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	if *version == "" {
		return errors.New("--version is required")
	}
	defs, err := loadDefinitions(*defsDir)
	if err != nil {
		return err
	}
	bundle := toolspec.Bundle{
		SchemaVersion: *schemaVersion,
		BundleVersion: *version,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Tools:         defs,
	}
	if err := bundle.Validate(); err != nil {
		return err
	}
	content, err := marshalBundle(bundle, *pretty)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, *out)
}

func loadDefinitions(dir string) ([]toolspec.ToolDefinition, error) {
	pattern := filepath.Join(dir, "*.tool.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no tool definitions under %s", dir)
	}
	sort.Strings(matches)
	defs := make([]toolspec.ToolDefinition, 0, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var def toolspec.ToolDefinition
		if err := json.Unmarshal(raw, &def); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := def.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defs = append(defs, def)
	}
	sort.SliceStable(defs, func(i, j int) bool {
		return strings.Compare(defs[i].Function.Name, defs[j].Function.Name) < 0
	})
	return defs, nil
}

func marshalBundle(bundle toolspec.Bundle, pretty bool) ([]byte, error) {
	if pretty {
		return json.MarshalIndent(bundle, "", "  ")
	}
	return json.Marshal(bundle)
}

func runPrintEnv(args []string) error {
	fs := flag.NewFlagSet("print-env", flag.ExitOnError)
	envKey := fs.String("env", "AGENT_TOOL_SPEC", "environment variable name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := toolspec.LoadBundleFromEnv(*envKey)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
