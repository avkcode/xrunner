package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type runbookDocument struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   runbookMeta   `yaml:"metadata"`
	Spec       runbookSpec   `yaml:"spec"`
	Steps      []runbookStep `yaml:"steps"` // legacy: allow top-level steps
	_          struct{}      `yaml:",inline"`
}

type runbookMeta struct {
	Name string `yaml:"name"`
}

type runbookSpec struct {
	Steps []runbookStep `yaml:"steps"`
}

type runbookStep struct {
	Name           string            `yaml:"name"`
	Type           string            `yaml:"type"`
	Host           string            `yaml:"host"`
	Script         string            `yaml:"script"`
	TimeoutSeconds int               `yaml:"timeoutSeconds"`
	SSHArgs        []string          `yaml:"sshArgs"`
	Env            map[string]string `yaml:"env"`
}

type runbookReport struct {
	Runbook string            `json:"runbook"`
	Started time.Time         `json:"startedAt"`
	Ended   time.Time         `json:"endedAt"`
	Success bool              `json:"success"`
	Steps   []runbookStepRun  `json:"steps"`
	Meta    map[string]string `json:"meta,omitempty"`
}

type runbookStepRun struct {
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Host       string    `json:"host,omitempty"`
	Started    time.Time `json:"startedAt"`
	Ended      time.Time `json:"endedAt"`
	DurationMS int64     `json:"durationMs"`
	ExitCode   int       `json:"exitCode"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	StdoutTail string    `json:"stdoutTail,omitempty"`
	StderrTail string    `json:"stderrTail,omitempty"`
}

type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	if t.max <= 0 {
		return len(p), nil
	}
	if len(p) >= t.max {
		t.b = append(t.b[:0], p[len(p)-t.max:]...)
		return len(p), nil
	}
	if len(t.b)+len(p) <= t.max {
		t.b = append(t.b, p...)
		return len(p), nil
	}
	overflow := len(t.b) + len(p) - t.max
	copy(t.b, t.b[overflow:])
	t.b = t.b[:len(t.b)-overflow]
	t.b = append(t.b, p...)
	return len(p), nil
}

func (t *tailBuffer) String() string {
	return string(t.b)
}

func newRunbookCmd(root *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runbook",
		Short: "Run ops runbooks",
	}
	cmd.AddCommand(newRunbookApplyCmd(root))
	return cmd
}

func newRunbookApplyCmd(_ *rootOptions) *cobra.Command {
	var confirm bool
	var dryRun bool
	var follow bool
	var jsonOut bool
	var reportPath string
	var sshArgs []string
	var sshShell string
	var captureBytes int

	cmd := &cobra.Command{
		Use:   "apply <runbook.yaml>",
		Short: "Apply a runbook (defaults to dry-run unless --confirm)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			doc, err := loadRunbook(path)
			if err != nil {
				return err
			}
			steps := doc.Spec.Steps
			if len(steps) == 0 {
				steps = doc.Steps
			}
			if err := validateRunbook(doc, steps); err != nil {
				return err
			}

			name := strings.TrimSpace(doc.Metadata.Name)
			if name == "" {
				name = filepath.Base(path)
			}

			if !confirm {
				dryRun = true
			}

			if dryRun {
				for i, step := range steps {
					fmt.Fprintf(os.Stdout, "%d. %s (%s)\n", i+1, step.Name, stepType(step))
					if strings.EqualFold(stepType(step), "ssh") {
						fmt.Fprintf(os.Stdout, "   host: %s\n", step.Host)
						fmt.Fprintf(os.Stdout, "   shell: %s -se\n", sshShell)
						fmt.Fprintf(os.Stdout, "   scriptBytes: %d\n", len(step.Script))
					}
				}
				fmt.Fprintln(os.Stdout, "dry-run: no commands executed (pass --confirm to run)")
				return nil
			}

			report := runbookReport{
				Runbook: name,
				Started: time.Now(),
				Steps:   make([]runbookStepRun, 0, len(steps)),
			}

			success := true
			for _, step := range steps {
				stepRun := runbookStepRun{
					Name:    step.Name,
					Type:    stepType(step),
					Host:    strings.TrimSpace(step.Host),
					Started: time.Now(),
				}

				stepTimeout := time.Duration(step.TimeoutSeconds) * time.Second
				ctx := context.Background()
				cancel := func() {}
				if stepTimeout > 0 {
					ctx, cancel = context.WithTimeout(ctx, stepTimeout)
				}

				stdoutTail := &tailBuffer{max: captureBytes}
				stderrTail := &tailBuffer{max: captureBytes}

				var stdout io.Writer = stdoutTail
				var stderr io.Writer = stderrTail
				if follow && !jsonOut {
					stdout = io.MultiWriter(os.Stdout, stdoutTail)
					stderr = io.MultiWriter(os.Stderr, stderrTail)
				}

				exitCode, runErr := runStep(ctx, step, sshShell, append([]string(nil), sshArgs...), stdout, stderr)
				cancel()

				stepRun.Ended = time.Now()
				stepRun.DurationMS = stepRun.Ended.Sub(stepRun.Started).Milliseconds()
				stepRun.ExitCode = exitCode
				stepRun.Success = runErr == nil && exitCode == 0
				stepRun.StdoutTail = sanitizeForJSON(stdoutTail.String())
				stepRun.StderrTail = sanitizeForJSON(stderrTail.String())
				if runErr != nil {
					stepRun.Error = runErr.Error()
				}

				report.Steps = append(report.Steps, stepRun)
				if !stepRun.Success {
					success = false
					break
				}
			}

			report.Ended = time.Now()
			report.Success = success

			if reportPath != "" {
				if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
					return err
				}
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(reportPath, data, 0o644); err != nil {
					return err
				}
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else {
				if report.Success {
					fmt.Fprintf(os.Stderr, "runbook succeeded: %s\n", report.Runbook)
				} else {
					fmt.Fprintf(os.Stderr, "runbook failed: %s\n", report.Runbook)
				}
			}

			if !report.Success {
				return errors.New("runbook failed")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&confirm, "confirm", false, "execute the runbook (otherwise prints the plan)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and exit (same as omitting --confirm)")
	cmd.Flags().BoolVar(&follow, "follow", true, "stream step output to stdout/stderr")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON report to stdout (disables --follow)")
	cmd.Flags().StringVar(&reportPath, "report", "", "write a JSON report to this path")
	cmd.Flags().StringArrayVar(&sshArgs, "ssh-arg", nil, "extra ssh argument (repeatable)")
	cmd.Flags().StringVar(&sshShell, "ssh-shell", "sh", "remote shell used for ssh steps")
	cmd.Flags().IntVar(&captureBytes, "capture-bytes", 64*1024, "tail bytes captured per stream in the report")
	return cmd
}

func stepType(step runbookStep) string {
	if strings.TrimSpace(step.Type) != "" {
		return strings.ToLower(strings.TrimSpace(step.Type))
	}
	return "ssh"
}

func loadRunbook(path string) (*runbookDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc runbookDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse runbook: %w", err)
	}
	return &doc, nil
}

func validateRunbook(doc *runbookDocument, steps []runbookStep) error {
	if doc == nil {
		return fmt.Errorf("runbook is nil")
	}
	if len(steps) == 0 {
		return fmt.Errorf("runbook has no steps")
	}
	for i, step := range steps {
		if strings.TrimSpace(step.Name) == "" {
			return fmt.Errorf("step %d: name is required", i+1)
		}
		switch stepType(step) {
		case "ssh":
			if strings.TrimSpace(step.Host) == "" {
				return fmt.Errorf("step %q: host is required for ssh steps", step.Name)
			}
			if strings.TrimSpace(step.Script) == "" {
				return fmt.Errorf("step %q: script is required for ssh steps", step.Name)
			}
		default:
			return fmt.Errorf("step %q: unsupported type %q", step.Name, step.Type)
		}
	}
	return nil
}

func runStep(ctx context.Context, step runbookStep, sshShell string, globalSSHArgs []string, stdout, stderr io.Writer) (int, error) {
	switch stepType(step) {
	case "ssh":
		return runSSH(ctx, step, sshShell, globalSSHArgs, stdout, stderr)
	default:
		return 1, fmt.Errorf("unsupported step type %q", step.Type)
	}
}

func runSSH(ctx context.Context, step runbookStep, sshShell string, globalSSHArgs []string, stdout, stderr io.Writer) (int, error) {
	host := strings.TrimSpace(step.Host)
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
	}
	args = append(args, globalSSHArgs...)
	args = append(args, step.SSHArgs...)
	args = append(args, host, sshShell, "-se")

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = strings.NewReader(applyEnvPrefix(step.Env) + step.Script)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ProcessState.ExitCode(), err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 124, fmt.Errorf("timeout: %w", err)
	}
	return 1, err
}

func applyEnvPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range env {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellEscape(v))
		b.WriteString("\n")
	}
	return b.String()
}

func shellEscape(value string) string {
	if value == "" {
		return "''"
	}
	// POSIX-ish single-quote escaping: ' -> '"'"'
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func sanitizeForJSON(s string) string {
	// Ensure the report stays readable if binaries leak into logs.
	return strings.ToValidUTF8(s, "\uFFFD")
}
