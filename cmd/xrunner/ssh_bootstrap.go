package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	cliconfig "github.com/antonkrylov/xrunner/internal/cli/config"
	"github.com/antonkrylov/xrunner/internal/llm"
)

type sshBootstrapFlags struct {
	repoDir        string
	binDir         string
	sshArgs        []string
	dryRun         bool
	installDeps    bool
	goarch         string
	remoteTmpBase  string
	writeConfig    bool
	llmFromEnv     bool
	llmFromEnvFile string
	installMode    string
	verifyLLM      bool
	verifyPrompt   string
	verifyTimeout  time.Duration
}

func newSSHBootstrapCmd() *cobra.Command {
	var f sshBootstrapFlags

	cmd := &cobra.Command{
		Use:   "bootstrap <user@host>",
		Short: "Install xrunner-api/worker/remote on a host via SSH (systemd + scp)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := strings.TrimSpace(args[0])
			if host == "" {
				return fmt.Errorf("host is required")
			}
			if strings.TrimSpace(f.remoteTmpBase) == "" {
				f.remoteTmpBase = "/tmp"
			}

			if f.dryRun {
				fmt.Fprintln(os.Stdout, "bootstrap plan (dry-run):")
				fmt.Fprintf(os.Stdout, "- build Linux payload (api/worker/remote)\n")
				fmt.Fprintf(os.Stdout, "- scp payload to %s:%s\n", host, f.remoteTmpBase)
				switch strings.ToLower(strings.TrimSpace(f.installMode)) {
				case "", "systemd":
					fmt.Fprintf(os.Stdout, "- install systemd units + start services (xrunner-worker, xrunner-api, xrunner-remote)\n")
				case "user":
					fmt.Fprintf(os.Stdout, "- install into ~/.xrunner and start api/worker/remote via nohup (no systemd)\n")
				default:
					fmt.Fprintf(os.Stdout, "- install mode: %s\n", strings.TrimSpace(f.installMode))
				}
				if f.llmFromEnv || strings.TrimSpace(f.llmFromEnvFile) != "" {
					if strings.ToLower(strings.TrimSpace(f.installMode)) == "user" {
						fmt.Fprintf(os.Stdout, "- write LLM env (XR_LLM_*) into ~/.xrunner/env/xrunner-remote\n")
					} else {
						fmt.Fprintf(os.Stdout, "- write LLM env (XR_LLM_*) into /etc/default/xrunner-remote and restart xrunner-remote\n")
					}
				}
				if f.verifyLLM {
					fmt.Fprintf(os.Stdout, "- verify LLM via ssh-forwarded prompt (timeout=%s)\n", f.verifyTimeout.String())
				}
				fmt.Fprintf(os.Stdout, "- optionally write sshHost into %s\n", effectiveConfigPath(cmd))
				fmt.Fprintln(os.Stdout, "pass --dry-run=false to execute")
				return nil
			}

			ctx := cmd.Context()
			if err := requireLocalTool("ssh"); err != nil {
				return err
			}
			if err := requireLocalTool("scp"); err != nil {
				return err
			}
			if strings.TrimSpace(f.binDir) == "" {
				if err := requireLocalTool("go"); err != nil {
					return err
				}
			}

			arch, err := resolveGOARCH(ctx, host, f.goarch, f.sshArgs)
			if err != nil {
				return err
			}

			payloadTGZ, cleanup, err := buildBootstrapPayload(ctx, arch, f)
			if err != nil {
				return err
			}
			defer cleanup()

			llmEnv, err := llmEnvForBootstrap(f.llmFromEnv, f.llmFromEnvFile)
			if err != nil {
				return err
			}
			if len(llmEnv) > 0 {
				if strings.ToLower(strings.TrimSpace(f.installMode)) == "user" {
					fmt.Fprintln(os.Stderr, "[xrunner] WARNING: bootstrap will persist your LLM API key in ~/.xrunner/env/xrunner-remote (mode 0600) on the remote host.")
				} else {
					fmt.Fprintln(os.Stderr, "[xrunner] WARNING: bootstrap will persist your LLM API key in /etc/default/xrunner-remote (mode 0600) on the remote host.")
				}
			}

			remoteTag := fmt.Sprintf("xrunner-bootstrap-%d", time.Now().Unix())
			remoteTGZ := fmt.Sprintf("%s/%s.tgz", strings.TrimRight(f.remoteTmpBase, "/"), remoteTag)
			remoteDir := fmt.Sprintf("%s/%s", strings.TrimRight(f.remoteTmpBase, "/"), remoteTag)

			fmt.Fprintf(os.Stdout, "[xrunner] uploading payload to %s...\n", host)
			if err := runSCP(ctx, payloadTGZ, fmt.Sprintf("%s:%s", host, remoteTGZ), f.sshArgs); err != nil {
				return err
			}

			fmt.Fprintf(os.Stdout, "[xrunner] installing on %s...\n", host)
			script, err := renderRemoteInstallScript(remoteTGZ, remoteDir, f.installDeps, llmEnv, f.installMode)
			if err != nil {
				return err
			}
			allowSudo := strings.ToLower(strings.TrimSpace(f.installMode)) != "user"
			if err := runRemoteScript(ctx, host, script, f.sshArgs, allowSudo); err != nil {
				return err
			}

			if f.verifyLLM {
				vTimeout := f.verifyTimeout
				if vTimeout <= 0 {
					vTimeout = 3 * time.Minute
				}
				prompt := strings.TrimSpace(f.verifyPrompt)
				if prompt == "" {
					prompt = "2+2"
				}
				vctx, vcancel := context.WithTimeout(ctx, vTimeout)
				defer vcancel()
				if err := verifyRemotePrompt(vctx, host, f.sshArgs, prompt); err != nil {
					return fmt.Errorf("verify-llm failed: %w", err)
				}
			}

			if f.writeConfig {
				if err := writeBootstrapContext(cmd, host); err != nil {
					return err
				}
			}

			fmt.Fprintf(os.Stdout, "[xrunner] done: ssh attach/chat now works against %s (xrunner-remote on 127.0.0.1:7337).\n", host)
			exe, _ := os.Executable()
			exe = strings.TrimSpace(exe)
			if exe == "" {
				exe = "xrunner"
			}
			if f.writeConfig {
				fmt.Fprintf(os.Stdout, "try: %s ssh chat\n", exe)
			} else {
				fmt.Fprintf(os.Stdout, "try: %s ssh chat --host %s\n", exe, host)
			}
			return nil
		},
	}
	cmd.SilenceUsage = true

	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what would happen without executing")
	cmd.Flags().StringVar(&f.repoDir, "repo", ".", "repo root used for builds when --bin-dir is unset")
	cmd.Flags().StringVar(&f.binDir, "bin-dir", "", "use prebuilt Linux binaries from this dir (xrunner-api/xrunner-worker/xrunner-remote)")
	cmd.Flags().StringArrayVar(&f.sshArgs, "ssh-arg", nil, "extra args passed to ssh/scp (repeatable, e.g. --ssh-arg=-i --ssh-arg=~/.ssh/id_ed25519)")
	cmd.Flags().BoolVar(&f.installDeps, "install-deps", true, "attempt to install tmux (apt-get) for durable shells")
	cmd.Flags().StringVar(&f.goarch, "arch", "", "override GOARCH for builds (amd64/arm64); auto-detected from remote uname -m when unset")
	cmd.Flags().StringVar(&f.remoteTmpBase, "remote-tmp", "/tmp", "remote temp directory for staging the payload")
	cmd.Flags().BoolVar(&f.writeConfig, "write-config", false, "write sshHost into the selected context in $HOME/.xrunner/config")
	cmd.Flags().BoolVar(&f.llmFromEnv, "llm-from-env", false, "write XR_LLM_* config into /etc/default/xrunner-remote (stores the API key on the host)")
	cmd.Flags().StringVar(&f.llmFromEnvFile, "llm-from-env-file", "", "read XR_LLM_* config from a local env file and write it into /etc/default/xrunner-remote (stores the API key on the host)")
	cmd.Flags().StringVar(&f.installMode, "install-mode", "systemd", "install mode: systemd|user (user installs to ~/.xrunner and uses nohup instead of systemd)")
	cmd.Flags().BoolVar(&f.verifyLLM, "verify-llm", false, "after install, send a test prompt through xrunner-remote and fail if no assistant answer is produced")
	cmd.Flags().StringVar(&f.verifyPrompt, "verify-prompt", "2+2", "prompt used for --verify-llm")
	cmd.Flags().DurationVar(&f.verifyTimeout, "verify-timeout", 3*time.Minute, "timeout for --verify-llm")
	return cmd
}

func quoteEnvValue(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

func validateOpenAICompatBaseURL(baseURL string, provider string, chatPath string) error {
	return llm.ValidateOpenAICompatBaseURL(baseURL, provider, chatPath)
}

func llmEnvForBootstrap(fromEnv bool, envFile string) ([]string, error) {
	if fromEnv && strings.TrimSpace(envFile) != "" {
		return nil, fmt.Errorf("--llm-from-env and --llm-from-env-file are mutually exclusive")
	}
	if strings.TrimSpace(envFile) != "" {
		return llmEnvForBootstrapFromFile(envFile)
	}
	if !fromEnv {
		return nil, nil
	}

	// Canonicalize config to XR_LLM_* so remote startup doesn't depend on OPENAI_* fallbacks.
	baseURL := strings.TrimSpace(os.Getenv("XR_LLM_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	}
	apiKey := strings.TrimSpace(os.Getenv("XR_LLM_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	model := strings.TrimSpace(os.Getenv("XR_LLM_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	}
	provider := strings.TrimSpace(os.Getenv("XR_LLM_PROVIDER"))
	if provider == "" {
		provider = "openai_compat"
	}
	chatPath := strings.TrimSpace(os.Getenv("XR_LLM_CHAT_PATH"))
	if err := validateOpenAICompatBaseURL(baseURL, provider, chatPath); err != nil {
		return nil, err
	}

	missing := []string{}
	if baseURL == "" {
		missing = append(missing, "XR_LLM_BASE_URL (or OPENAI_BASE_URL)")
	}
	if apiKey == "" {
		missing = append(missing, "XR_LLM_API_KEY (or OPENAI_API_KEY)")
	}
	if model == "" {
		missing = append(missing, "XR_LLM_MODEL (or OPENAI_MODEL)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("--llm-from-env: missing %s", strings.Join(missing, ", "))
	}

	lines := []string{
		"XR_LLM_ENABLED=1",
		"XR_LLM_PROVIDER=" + quoteEnvValue(provider),
		"XR_LLM_BASE_URL=" + quoteEnvValue(baseURL),
		"XR_LLM_API_KEY=" + quoteEnvValue(apiKey),
		"XR_LLM_MODEL=" + quoteEnvValue(model),
	}

	// Optional knobs: copy through if set locally.
	for _, k := range []string{
		"XR_LLM_TIMEOUT_MS",
		"XR_LLM_STREAM",
		"XR_LLM_MAX_TOOL_ITERS",
		"XR_LLM_TOOL_OUTPUT_MAX_BYTES",
		"XR_LLM_MAX_HISTORY_EVENTS",
		"XR_LLM_SYSTEM",
		"XR_LLM_CHAT_PATH",
		"XR_LLM_API_KEY_HEADER",
		"XR_LLM_API_KEY_PREFIX",
		"XR_LLM_GEMINI_KEY_HEADER",
		"XR_LLM_GEMINI_OPERATOR",
	} {
		if v := os.Getenv(k); strings.TrimSpace(v) != "" {
			lines = append(lines, k+"="+quoteEnvValue(v))
		}
	}

	// Keep the remote env file line-oriented.
	for _, l := range lines {
		if strings.ContainsAny(l, "\n\r\x00") {
			return nil, fmt.Errorf("--llm-from-env: env contains invalid characters (newline/NUL)")
		}
	}
	return lines, nil
}

func llmEnvForBootstrapFromFile(path string) ([]string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return nil, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("--llm-from-env-file: %w", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	kv := map[string]string{}
	for _, raw := range lines {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		// Drop surrounding quotes for common formats.
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if k == "" {
			continue
		}
		kv[k] = v
	}

	get := func(k string) string { return strings.TrimSpace(kv[k]) }
	baseURL := get("XR_LLM_BASE_URL")
	if baseURL == "" {
		baseURL = get("OPENAI_BASE_URL")
	}
	apiKey := get("XR_LLM_API_KEY")
	if apiKey == "" {
		apiKey = get("OPENAI_API_KEY")
	}
	model := get("XR_LLM_MODEL")
	if model == "" {
		model = get("OPENAI_MODEL")
	}
	provider := get("XR_LLM_PROVIDER")
	if provider == "" {
		provider = "openai_compat"
	}
	chatPath := get("XR_LLM_CHAT_PATH")
	if err := validateOpenAICompatBaseURL(baseURL, provider, chatPath); err != nil {
		return nil, err
	}

	missing := []string{}
	if baseURL == "" {
		missing = append(missing, "XR_LLM_BASE_URL (or OPENAI_BASE_URL)")
	}
	if apiKey == "" {
		missing = append(missing, "XR_LLM_API_KEY (or OPENAI_API_KEY)")
	}
	if model == "" {
		missing = append(missing, "XR_LLM_MODEL (or OPENAI_MODEL)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("--llm-from-env-file: missing %s", strings.Join(missing, ", "))
	}

	out := []string{
		"XR_LLM_ENABLED=1",
		"XR_LLM_PROVIDER=" + quoteEnvValue(provider),
		"XR_LLM_BASE_URL=" + quoteEnvValue(baseURL),
		"XR_LLM_API_KEY=" + quoteEnvValue(apiKey),
		"XR_LLM_MODEL=" + quoteEnvValue(model),
	}
	for _, k := range []string{
		"XR_LLM_TIMEOUT_MS",
		"XR_LLM_STREAM",
		"XR_LLM_MAX_TOOL_ITERS",
		"XR_LLM_TOOL_OUTPUT_MAX_BYTES",
		"XR_LLM_MAX_HISTORY_EVENTS",
		"XR_LLM_SYSTEM",
		"XR_LLM_CHAT_PATH",
		"XR_LLM_API_KEY_HEADER",
		"XR_LLM_API_KEY_PREFIX",
		"XR_LLM_GEMINI_KEY_HEADER",
		"XR_LLM_GEMINI_OPERATOR",
	} {
		if v := get(k); v != "" {
			out = append(out, k+"="+quoteEnvValue(v))
		}
	}

	for _, l := range out {
		if strings.ContainsAny(l, "\n\r\x00") {
			return nil, fmt.Errorf("--llm-from-env-file: env contains invalid characters (newline/NUL)")
		}
	}
	return out, nil
}

func effectiveConfigPath(cmd *cobra.Command) string {
	if cmd != nil && cmd.Root() != nil {
		if v, err := cmd.Root().PersistentFlags().GetString("config"); err == nil && strings.TrimSpace(v) != "" {
			return v
		}
	}
	if v := strings.TrimSpace(os.Getenv("XRUNNER_CONFIG")); v != "" {
		return v
	}
	return cliconfig.DefaultConfigPath()
}

func writeBootstrapContext(cmd *cobra.Command, sshHost string) error {
	if cmd == nil || cmd.Root() == nil {
		return fmt.Errorf("cannot resolve root flags for config write")
	}
	cfgPath := effectiveConfigPath(cmd)

	cfg, err := cliconfig.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &cliconfig.Config{}
	}

	ctxName, _ := cmd.Root().PersistentFlags().GetString("context")
	ctxName = strings.TrimSpace(ctxName)
	if ctxName == "" {
		// Prefer currentContext if present, otherwise fall back to the sole context if unambiguous.
		if v := strings.TrimSpace(cfg.CurrentContext); v != "" {
			ctxName = v
		} else if len(cfg.Contexts) == 1 {
			for k := range cfg.Contexts {
				ctxName = k
				break
			}
		}
	}
	if ctxName == "" {
		existing := []string{}
		for k := range cfg.Contexts {
			existing = append(existing, k)
		}
		if len(existing) > 0 {
			return fmt.Errorf("--context is required when --write-config is set (no currentContext in %s). Existing contexts: %s", cfgPath, strings.Join(existing, ", "))
		}
		return fmt.Errorf("--context is required when --write-config is set (no config/currentContext found at %s)", cfgPath)
	}

	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*cliconfig.Context{}
	}
	ctx := cfg.Contexts[ctxName]
	if ctx == nil {
		ctx = &cliconfig.Context{}
		cfg.Contexts[ctxName] = ctx
	}
	ctx.SSHHost = strings.TrimSpace(sshHost)

	// Also set a reasonable default API endpoint for `xrunner job ...` usage.
	hostPart := sshHost
	if i := strings.LastIndex(hostPart, "@"); i >= 0 {
		hostPart = hostPart[i+1:]
	}
	hostPart = strings.TrimSpace(hostPart)
	if hostPart != "" && strings.TrimSpace(ctx.Server) == "" {
		ctx.Server = hostPart + ":50051"
	}
	if ctx.TimeoutSeconds == 0 {
		ctx.TimeoutSeconds = 20
	}
	cfg.CurrentContext = ctxName

	if err := cfg.Save(cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "[xrunner] wrote sshHost to %s (context %s)\n", cfgPath, ctxName)
	return nil
}

func requireLocalTool(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("missing required local tool %q in PATH", name)
	}
	return nil
}

func resolveGOARCH(ctx context.Context, host, override string, sshArgs []string) (string, error) {
	if v := strings.TrimSpace(override); v != "" {
		switch v {
		case "amd64", "arm64":
			return v, nil
		default:
			return "", fmt.Errorf("unsupported --arch %q (expected amd64 or arm64)", v)
		}
	}
	out, err := runSSHCapture(ctx, host, append(sshArgs, "uname -m")...)
	if err != nil {
		return "", err
	}
	m := strings.TrimSpace(out)
	switch m {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote arch %q (use --arch to override)", m)
	}
}

func buildBootstrapPayload(ctx context.Context, arch string, f sshBootstrapFlags) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "xrunner-bootstrap-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	repo, err := filepath.Abs(f.repoDir)
	if err != nil {
		cleanup()
		return "", nil, err
	}

	payloadRoot := filepath.Join(tmp, "payload")
	if err := os.MkdirAll(filepath.Join(payloadRoot, "bin"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Join(payloadRoot, "systemd"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Join(payloadRoot, "configs"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}

	// Binaries.
	if strings.TrimSpace(f.binDir) != "" {
		apiSrc := filepath.Join(f.binDir, "xrunner-api")
		workerSrc := filepath.Join(f.binDir, "xrunner-worker")
		remoteSrc := filepath.Join(f.binDir, "xrunner-remote")
		if err := validateLinuxELFBin(apiSrc, arch); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := validateLinuxELFBin(workerSrc, arch); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := validateLinuxELFBin(remoteSrc, arch); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := copyRequiredBin(apiSrc, filepath.Join(payloadRoot, "bin", "xrunner-api")); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := copyRequiredBin(workerSrc, filepath.Join(payloadRoot, "bin", "xrunner-worker")); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := copyRequiredBin(remoteSrc, filepath.Join(payloadRoot, "bin", "xrunner-remote")); err != nil {
			cleanup()
			return "", nil, err
		}
	} else {
		if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("--repo %q does not look like a Go module root (missing go.mod)", repo)
		}
		if err := goBuild(ctx, repo, arch, "./cmd/api", filepath.Join(payloadRoot, "bin", "xrunner-api")); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := goBuild(ctx, repo, arch, "./cmd/worker", filepath.Join(payloadRoot, "bin", "xrunner-worker")); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := goBuild(ctx, repo, arch, "./cmd/xrunner-remote", filepath.Join(payloadRoot, "bin", "xrunner-remote")); err != nil {
			cleanup()
			return "", nil, err
		}
	}

	// Assets (repo files).
	if err := copyRequiredAsset(filepath.Join(repo, "configs/default.nsjail.cfg"), filepath.Join(payloadRoot, "configs/default.nsjail.cfg"), 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	for _, p := range []string{
		"deploy/systemd/xrunner-api.service",
		"deploy/systemd/xrunner-worker.service",
		"deploy/systemd/xrunner-remote.service",
		"deploy/systemd/xrunner-api.env",
		"deploy/systemd/xrunner-worker.env",
		"deploy/systemd/xrunner-remote.env",
	} {
		dst := strings.TrimPrefix(p, "deploy/systemd/")
		if err := copyRequiredAsset(filepath.Join(repo, p), filepath.Join(payloadRoot, "systemd", dst), 0o644); err != nil {
			cleanup()
			return "", nil, err
		}
	}

	outTGZ := filepath.Join(tmp, "xrunner-bootstrap.tgz")
	if err := writeTGZ(outTGZ, payloadRoot); err != nil {
		cleanup()
		return "", nil, err
	}
	return outTGZ, cleanup, nil
}

func validateLinuxELFBin(path string, goarch string) error {
	in, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	ef, err := elf.NewFile(in)
	if err != nil {
		return fmt.Errorf(
			"%s is not a Linux ELF binary (bootstrap expects Linux binaries; omit --bin-dir to cross-compile automatically): %w",
			path,
			err,
		)
	}
	want := elf.Machine(0)
	switch strings.ToLower(strings.TrimSpace(goarch)) {
	case "amd64":
		want = elf.EM_X86_64
	case "arm64":
		want = elf.EM_AARCH64
	default:
		return fmt.Errorf("unsupported GOARCH for --bin-dir validation: %q", goarch)
	}
	if ef.FileHeader.Machine != want {
		return fmt.Errorf("%s ELF arch mismatch: got %v, want %v (GOARCH=%s)", path, ef.FileHeader.Machine, want, goarch)
	}
	return nil
}

func startTunnelWithSSHArgs(ctx context.Context, host string, remotePort int, sshArgs []string) (*sshTunnel, error) {
	localPort, err := freeLocalPort()
	if err != nil {
		return nil, err
	}
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	args := []string{"-o", "ExitOnForwardFailure=yes"}
	args = append(args, sshArgs...)
	args = append(args,
		"-L", fmt.Sprintf("%s:127.0.0.1:%d", localAddr, remotePort),
		"-N", host,
	)
	fwd := exec.CommandContext(ctx, "ssh", args...)
	fwd.Stdout = os.Stderr
	fwd.Stderr = os.Stderr
	if err := fwd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh forward: %w", err)
	}
	return &sshTunnel{localAddr: localAddr, proc: fwd}, nil
}

func verifyRemotePrompt(ctx context.Context, host string, sshArgs []string, prompt string) error {
	tunnel, err := startTunnelWithSSHArgs(ctx, host, 7337, sshArgs)
	if err != nil {
		return err
	}
	defer tunnel.Close()

	conn, err := dialRemoteOnce(ctx, tunnel.localAddr, 4*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctrl := xrunnerv1.NewRemoteControlServiceClient(conn)
	if _, err := ctrl.Ping(ctx, &emptypb.Empty{}); err != nil {
		return err
	}

	ss := xrunnerv1.NewSessionServiceClient(conn)
	created, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{WorkspaceRoot: "/", ToolBundlePath: ""})
	if err != nil {
		return err
	}
	sid := created.GetSession().GetSessionId()
	if strings.TrimSpace(sid) == "" {
		return fmt.Errorf("verify: empty session id")
	}

	stream, err := ss.Interact(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.CloseSend() }()
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{SessionId: sid}}}); err != nil {
		return err
	}
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: prompt}}}); err != nil {
		return err
	}

	_ = protojson.MarshalOptions{UseProtoNames: true}
	var assistantText strings.Builder
	sawAssistant := false
	for {
		ev, err := stream.Recv()
		if err != nil {
			return err
		}
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_ToolOutputChunk:
			if k.ToolOutputChunk != nil && k.ToolOutputChunk.GetStream() == "assistant" {
				assistantText.Write(k.ToolOutputChunk.GetData())
				sawAssistant = true
			}
		case *xrunnerv1.AgentEvent_AssistantMessage:
			if k.AssistantMessage != nil {
				assistantText.Reset()
				assistantText.WriteString(k.AssistantMessage.GetText())
				sawAssistant = true
			}
		case *xrunnerv1.AgentEvent_Error:
			if k.Error != nil && strings.TrimSpace(k.Error.GetMessage()) != "" {
				return fmt.Errorf("verify: %s", strings.TrimSpace(k.Error.GetMessage()))
			}
		case *xrunnerv1.AgentEvent_Status:
			if k.Status != nil && strings.TrimSpace(k.Status.GetPhase()) == "idle" && sawAssistant {
				out := strings.TrimSpace(assistantText.String())
				if out == "" || out == "ack" {
					return fmt.Errorf("verify: unexpected assistant output %q", out)
				}
				return nil
			}
		default:
		}
	}
}

func copyRequiredAsset(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read asset %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

func copyRequiredBin(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o755)
}

func goBuild(ctx context.Context, repo, arch, pkg, outPath string) error {
	c := exec.CommandContext(ctx, "go", "build", "-o", outPath, pkg)
	c.Dir = repo
	c.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+arch,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build %s: %w\n%s", pkg, err, out)
	}
	return nil
}

func writeTGZ(outPath, dir string) error {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = "payload/" + rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

func runSCP(ctx context.Context, src, dst string, sshArgs []string) error {
	args := append([]string(nil), sshArgs...)
	args = append(args, src, dst)
	c := exec.CommandContext(ctx, "scp", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runSSHCapture(ctx context.Context, host string, sshArgs ...string) (string, error) {
	args := append([]string(nil), sshArgs...)
	// If the caller already includes options, keep them; always add host before the command.
	if len(args) == 0 {
		return "", errors.New("ssh args missing remote command")
	}
	cmdStr := args[len(args)-1]
	base := args[:len(args)-1]
	full := append([]string(nil), base...)
	full = append(full, host, cmdStr)
	c := exec.CommandContext(ctx, "ssh", full...)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh %s %q: %w\n%s", host, cmdStr, err, out)
	}
	return string(out), nil
}

func runRemoteScript(ctx context.Context, host string, script []byte, sshArgs []string, allowSudo bool) error {
	// Execute as root if possible, otherwise prompt for sudo over a tty.
	uidOut, err := runSSHRawCapture(ctx, host, append(sshArgs, "id -u")...)
	if err != nil {
		return err
	}
	uid := strings.TrimSpace(uidOut)
	useSudo := allowSudo && uid != "0"

	remoteCmd := "bash -se"
	var args []string
	if useSudo {
		args = append(args, "-t")
		remoteCmd = "sudo bash -se"
	}
	args = append(args, sshArgs...)
	args = append(args, host, remoteCmd)

	c := exec.CommandContext(ctx, "ssh", args...)
	c.Stdin = bytes.NewReader(script)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("remote install failed (host=%s): %w", host, err)
	}
	return nil
}

func runSSHRawCapture(ctx context.Context, host string, sshArgs ...string) (string, error) {
	args := append([]string(nil), sshArgs...)
	if len(args) == 0 {
		return "", errors.New("ssh args missing remote command")
	}
	cmdStr := args[len(args)-1]
	base := args[:len(args)-1]
	full := append([]string(nil), base...)
	full = append(full, host, cmdStr)
	c := exec.CommandContext(ctx, "ssh", full...)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh %s %q: %w\n%s", host, cmdStr, err, out)
	}
	return string(out), nil
}

func renderRemoteInstallScript(remoteTGZ, remoteDir string, installDeps bool, llmEnv []string, installMode string) ([]byte, error) {
	mode := strings.ToLower(strings.TrimSpace(installMode))
	switch mode {
	case "", "systemd":
		return renderRemoteSystemdInstallScript(remoteTGZ, remoteDir, installDeps, llmEnv)
	case "user":
		return renderRemoteUserInstallScript(remoteTGZ, remoteDir, llmEnv)
	default:
		return nil, fmt.Errorf("unsupported --install-mode: %q (expected systemd|user)", strings.TrimSpace(installMode))
	}
}

func renderRemoteUserInstallScript(remoteTGZ, remoteDir string, llmEnv []string) ([]byte, error) {
	var b bytes.Buffer
	_, _ = b.WriteString("set -euo pipefail\n")
	_, _ = b.WriteString("umask 077\n")
	_, _ = b.WriteString("BOOTSTRAP_START=$(date -Is)\n")
	_, _ = b.WriteString("if [ \"$(uname -s)\" != \"Linux\" ]; then echo \"[xrunner] Linux only\" >&2; exit 1; fi\n")
	_, _ = b.WriteString("BASE=\"$HOME/.xrunner\"\n")
	_, _ = b.WriteString("BIN=\"$BASE/bin\"\n")
	_, _ = b.WriteString("CFG=\"$BASE/configs\"\n")
	_, _ = b.WriteString("LOG=\"$BASE/logs\"\n")
	_, _ = b.WriteString("ENV_DIR=\"$BASE/env\"\n")
	_, _ = b.WriteString("PIDS=\"$BASE/pids\"\n")
	_, _ = b.WriteString("WORK=\"$BASE/workspaces\"\n")
	_, _ = b.WriteString("SESS=\"$BASE/sessions\"\n")
	_, _ = b.WriteString("mkdir -p \"$BIN\" \"$CFG\" \"$LOG\" \"$ENV_DIR\" \"$PIDS\" \"$WORK\" \"$SESS\"\n")

	_, _ = b.WriteString("stop_pidfile() {\n")
	_, _ = b.WriteString("  f=\"$1\"\n")
	_, _ = b.WriteString("  [ -f \"$f\" ] || return 0\n")
	_, _ = b.WriteString("  pid=$(cat \"$f\" 2>/dev/null || true)\n")
	_, _ = b.WriteString("  if [ -n \"$pid\" ]; then kill \"$pid\" 2>/dev/null || true; fi\n")
	_, _ = b.WriteString("  for i in $(seq 1 25); do\n")
	_, _ = b.WriteString("    pid2=$(cat \"$f\" 2>/dev/null || true)\n")
	_, _ = b.WriteString("    [ -z \"$pid2\" ] && break\n")
	_, _ = b.WriteString("    kill -0 \"$pid2\" 2>/dev/null || break\n")
	_, _ = b.WriteString("    sleep 0.2\n")
	_, _ = b.WriteString("  done\n")
	_, _ = b.WriteString("  rm -f \"$f\"\n")
	_, _ = b.WriteString("}\n")

	_, _ = b.WriteString("stop_pidfile \"$PIDS/remote.pid\"\n")
	_, _ = b.WriteString("stop_pidfile \"$PIDS/api.pid\"\n")
	_, _ = b.WriteString("stop_pidfile \"$PIDS/worker.pid\"\n")

	_, _ = b.WriteString(fmt.Sprintf("rm -rf %s && mkdir -p %s\n", shQuote(remoteDir), shQuote(remoteDir)))
	_, _ = b.WriteString(fmt.Sprintf("tar -xzf %s -C %s\n", shQuote(remoteTGZ), shQuote(remoteDir)))
	_, _ = b.WriteString("SRC=" + shQuote(filepath.ToSlash(filepath.Join(remoteDir, "payload"))) + "\n")

	_, _ = b.WriteString("install -m 0755 \"$SRC/bin/xrunner-api\" \"$BIN/xrunner-api\"\n")
	_, _ = b.WriteString("install -m 0755 \"$SRC/bin/xrunner-worker\" \"$BIN/xrunner-worker\"\n")
	_, _ = b.WriteString("install -m 0755 \"$SRC/bin/xrunner-remote\" \"$BIN/xrunner-remote\"\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/configs/default.nsjail.cfg\" \"$CFG/default.nsjail.cfg\"\n")

	if len(llmEnv) > 0 {
		_, _ = b.WriteString("echo \"[xrunner] configuring LLM backend for user-mode xrunner-remote\" >&2\n")
		_, _ = b.WriteString("install -m 0600 /dev/null \"$ENV_DIR/xrunner-remote\"\n")
		_, _ = b.WriteString("cat >>\"$ENV_DIR/xrunner-remote\" <<'XRUNNER_LLM_ENV'\n")
		for _, l := range llmEnv {
			_, _ = b.WriteString(l)
			_, _ = b.WriteString("\n")
		}
		_, _ = b.WriteString("XRUNNER_LLM_ENV\n")
	}

	_, _ = b.WriteString("wait_port() {\n")
	_, _ = b.WriteString("  port=\"$1\"\n")
	_, _ = b.WriteString("  for i in $(seq 1 40); do (echo > /dev/tcp/127.0.0.1/${port}) >/dev/null 2>&1 && return 0; sleep 0.2; done\n")
	_, _ = b.WriteString("  return 1\n")
	_, _ = b.WriteString("}\n")

	_, _ = b.WriteString("nohup \"$BIN/xrunner-worker\" --listen 127.0.0.1:50052 --default-sandbox-type none --workspace \"$WORK\" --allow-unsafe-fallback=true >\"$LOG/worker.log\" 2>&1 & echo $! >\"$PIDS/worker.pid\"\n")
	_, _ = b.WriteString("wait_port 50052 || { echo \"[xrunner] worker did not start; tail $LOG/worker.log\" >&2; tail -n 120 \"$LOG/worker.log\" >&2 || true; exit 1; }\n")
	_, _ = b.WriteString("nohup \"$BIN/xrunner-api\" --listen 127.0.0.1:50051 --worker 127.0.0.1:50052 >\"$LOG/api.log\" 2>&1 & echo $! >\"$PIDS/api.pid\"\n")
	_, _ = b.WriteString("wait_port 50051 || { echo \"[xrunner] api did not start; tail $LOG/api.log\" >&2; tail -n 120 \"$LOG/api.log\" >&2 || true; exit 1; }\n")
	_, _ = b.WriteString("(\n")
	_, _ = b.WriteString("  set -a\n")
	_, _ = b.WriteString("  [ -f \"$ENV_DIR/xrunner-remote\" ] && . \"$ENV_DIR/xrunner-remote\"\n")
	_, _ = b.WriteString("  set +a\n")
	_, _ = b.WriteString("  nohup \"$BIN/xrunner-remote\" --listen 127.0.0.1:7337 --upstream 127.0.0.1:50051 --workspace-root \"$HOME\" --unsafe-allow-rootfs=false --sessions-dir \"$SESS\" >\"$LOG/remote.log\" 2>&1 & echo $! >\"$PIDS/remote.pid\"\n")
	_, _ = b.WriteString(")\n")
	_, _ = b.WriteString("wait_port 7337 || { echo \"[xrunner] remote did not start; tail $LOG/remote.log\" >&2; tail -n 120 \"$LOG/remote.log\" >&2 || true; exit 1; }\n")

	_, _ = b.WriteString("echo \"[xrunner] bootstrap ok (user mode)\" \n")
	_, _ = b.WriteString("echo \"[xrunner] ports: 50052(worker) 50051(api) 7337(remote)\" \n")
	_, _ = b.WriteString("echo \"[xrunner] logs: $LOG\" \n")
	_, _ = b.WriteString("echo \"[xrunner] pids: $PIDS\" \n")
	return b.Bytes(), nil
}

func renderRemoteSystemdInstallScript(remoteTGZ, remoteDir string, installDeps bool, llmEnv []string) ([]byte, error) {
	var b bytes.Buffer
	_, _ = b.WriteString("set -euo pipefail\n")
	_, _ = b.WriteString("umask 022\n")
	_, _ = b.WriteString("BOOTSTRAP_START=$(date -Is)\n")
	_, _ = b.WriteString("BOOTSTRAP_EPOCH=$(date +%s)\n")
	_, _ = b.WriteString("if [ \"$(uname -s)\" != \"Linux\" ]; then echo \"[xrunner] Linux only\" >&2; exit 1; fi\n")
	_, _ = b.WriteString("if ! command -v systemctl >/dev/null 2>&1; then echo \"[xrunner] systemd required (missing systemctl)\" >&2; exit 1; fi\n")
	_, _ = b.WriteString("stop_unit() { u=\"$1\"; systemctl stop \"$u\" >/dev/null 2>&1 || true; systemctl reset-failed \"$u\" >/dev/null 2>&1 || true; }\n")
	_, _ = b.WriteString("pid_on_port() {\n")
	_, _ = b.WriteString("  port=\"$1\"\n")
	_, _ = b.WriteString("  if command -v ss >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("    line=$(ss -ltnp 2>/dev/null | grep -E \"LISTEN.*:${port}[[:space:]]\" | head -n1 || true)\n")
	_, _ = b.WriteString("    if [ -n \"$line\" ]; then\n")
	_, _ = b.WriteString("      pid=$(echo \"$line\" | sed -n 's/.*pid=\\([0-9][0-9]*\\).*/\\1/p' | head -n1)\n")
	_, _ = b.WriteString("      if [ -n \"$pid\" ]; then echo \"$pid\"; return 0; fi\n")
	_, _ = b.WriteString("    fi\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  if command -v lsof >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("    lsof -nP -iTCP:\"$port\" -sTCP:LISTEN -t 2>/dev/null | head -n1 || true\n")
	_, _ = b.WriteString("    return 0\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  echo \"\"; return 0\n")
	_, _ = b.WriteString("}\n")
	_, _ = b.WriteString("ensure_port_free_for_xrunner_remote() {\n")
	_, _ = b.WriteString("  port=\"$1\"\n")
	_, _ = b.WriteString("  pid=$(pid_on_port \"$port\")\n")
	_, _ = b.WriteString("  if [ -z \"$pid\" ]; then return 0; fi\n")
	_, _ = b.WriteString("  comm=$(ps -p \"$pid\" -o comm= 2>/dev/null | tr -d ' ' || true)\n")
	_, _ = b.WriteString("  if [ \"$comm\" = \"xrunner-remote\" ]; then\n")
	_, _ = b.WriteString("    echo \"[xrunner] freeing 127.0.0.1:$port (killing pid=$pid comm=$comm)\" >&2\n")
	_, _ = b.WriteString("    kill \"$pid\" 2>/dev/null || true\n")
	_, _ = b.WriteString("    for i in $(seq 1 25); do pid2=$(pid_on_port \"$port\"); [ -z \"$pid2\" ] && return 0; sleep 0.2; done\n")
	_, _ = b.WriteString("    echo \"[xrunner] failed to stop xrunner-remote pid=$pid on 127.0.0.1:$port\" >&2\n")
	_, _ = b.WriteString("    return 1\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  echo \"[xrunner] 127.0.0.1:$port is already in use by pid=$pid comm=${comm:-unknown}; abort\" >&2\n")
	_, _ = b.WriteString("  if command -v ss >/dev/null 2>&1; then ss -ltnp 2>/dev/null | grep -E \"127\\\\.0\\\\.0\\\\.1:${port}\" >&2 || true; fi\n")
	_, _ = b.WriteString("  if command -v lsof >/dev/null 2>&1; then lsof -nP -iTCP:\"$port\" -sTCP:LISTEN >&2 || true; fi\n")
	_, _ = b.WriteString("  return 1\n")
	_, _ = b.WriteString("}\n")
	_, _ = b.WriteString("BACKUP_DIR=/var/lib/xrunner/backup/$BOOTSTRAP_EPOCH\n")
	_, _ = b.WriteString("backup_file() { src=\"$1\"; dst=\"$2\"; if [ -e \"$src\" ]; then install -D -m 0644 \"$src\" \"$BACKUP_DIR/$dst\"; fi }\n")
	_, _ = b.WriteString("backup_bin() { src=\"$1\"; dst=\"$2\"; if [ -x \"$src\" ]; then install -D -m 0755 \"$src\" \"$BACKUP_DIR/$dst\"; fi }\n")
	_, _ = b.WriteString("rollback() {\n")
	_, _ = b.WriteString("  if [ ! -d \"$BACKUP_DIR\" ]; then return 0; fi\n")
	_, _ = b.WriteString("  echo \"[xrunner] attempting rollback from $BACKUP_DIR\" >&2\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/bin/xrunner-api\" ]; then install -m 0755 \"$BACKUP_DIR/bin/xrunner-api\" \"$BIN_API\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/bin/xrunner-worker\" ]; then install -m 0755 \"$BACKUP_DIR/bin/xrunner-worker\" \"$BIN_WORKER\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/bin/xrunner-remote\" ]; then install -m 0755 \"$BACKUP_DIR/bin/xrunner-remote\" \"$BIN_REMOTE\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/etc/default/xrunner-api\" ]; then install -m 0644 \"$BACKUP_DIR/etc/default/xrunner-api\" \"$DEFAULTS_DIR/xrunner-api\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/etc/default/xrunner-worker\" ]; then install -m 0644 \"$BACKUP_DIR/etc/default/xrunner-worker\" \"$DEFAULTS_DIR/xrunner-worker\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/etc/default/xrunner-remote\" ]; then install -m 0600 \"$BACKUP_DIR/etc/default/xrunner-remote\" \"$DEFAULTS_DIR/xrunner-remote\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/etc/systemd/system/xrunner-api.service\" ]; then install -m 0644 \"$BACKUP_DIR/etc/systemd/system/xrunner-api.service\" \"$SYSTEMD_DIR/xrunner-api.service\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/etc/systemd/system/xrunner-worker.service\" ]; then install -m 0644 \"$BACKUP_DIR/etc/systemd/system/xrunner-worker.service\" \"$SYSTEMD_DIR/xrunner-worker.service\"; fi\n")
	_, _ = b.WriteString("  if [ -f \"$BACKUP_DIR/etc/systemd/system/xrunner-remote.service\" ]; then install -m 0644 \"$BACKUP_DIR/etc/systemd/system/xrunner-remote.service\" \"$SYSTEMD_DIR/xrunner-remote.service\"; fi\n")
	_, _ = b.WriteString("  systemctl daemon-reload || true\n")
	_, _ = b.WriteString("  systemctl restart xrunner-worker.service xrunner-api.service xrunner-remote.service >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("}\n")
	if installDeps {
		_, _ = b.WriteString("if command -v apt-get >/dev/null 2>&1; then\n")
		_, _ = b.WriteString("  export DEBIAN_FRONTEND=noninteractive\n")
		_, _ = b.WriteString("  apt-get update -y\n")
		_, _ = b.WriteString("  apt-get install -y tmux ncurses-term ca-certificates\n")
		_, _ = b.WriteString("fi\n")
	}
	_, _ = b.WriteString("REMOTE_LOG=\"$HOME/.xrunner/remote.log\"\n")
	_, _ = b.WriteString("mkdir -p \"$HOME/.xrunner\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("rm -f \"$REMOTE_LOG\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString(fmt.Sprintf("rm -rf %s && mkdir -p %s\n", shQuote(remoteDir), shQuote(remoteDir)))
	_, _ = b.WriteString(fmt.Sprintf("tar -xzf %s -C %s\n", shQuote(remoteTGZ), shQuote(remoteDir)))
	_, _ = b.WriteString("SRC=" + shQuote(filepath.ToSlash(filepath.Join(remoteDir, "payload"))) + "\n")
	_, _ = b.WriteString("XRUNNER_USER=xrunner\nXRUNNER_GROUP=xrunner\n")
	_, _ = b.WriteString("STATE_DIR=/var/lib/xrunner\nCONFIG_DIR=/etc/xrunner\nDEFAULTS_DIR=/etc/default\nSYSTEMD_DIR=/etc/systemd/system\n")
	_, _ = b.WriteString("BIN_API=/usr/local/bin/xrunner-api\nBIN_WORKER=/usr/local/bin/xrunner-worker\nBIN_REMOTE=/usr/local/bin/xrunner-remote\n")

	// Ensure clean upgrade: stop any existing services before replacing binaries/units.
	_, _ = b.WriteString("echo \"[xrunner] stopping existing services (if any)\" >&2\n")
	_, _ = b.WriteString("stop_unit xrunner-remote.service\n")
	_, _ = b.WriteString("stop_unit xrunner-api.service\n")
	_, _ = b.WriteString("stop_unit xrunner-worker.service\n")
	_, _ = b.WriteString("pkill -f \"xrunner-remote\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("pkill -f \"xrunner-api\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("pkill -f \"xrunner-worker\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("ensure_port_free_for_xrunner_remote 7337 || exit 1\n")

	_, _ = b.WriteString("echo \"[xrunner] backing up current install (best-effort)\" >&2\n")
	_, _ = b.WriteString("install -d -m 0700 \"$BACKUP_DIR\" || true\n")
	_, _ = b.WriteString("backup_bin \"$BIN_API\" \"bin/xrunner-api\"\n")
	_, _ = b.WriteString("backup_bin \"$BIN_WORKER\" \"bin/xrunner-worker\"\n")
	_, _ = b.WriteString("backup_bin \"$BIN_REMOTE\" \"bin/xrunner-remote\"\n")
	_, _ = b.WriteString("backup_file \"$DEFAULTS_DIR/xrunner-api\" \"etc/default/xrunner-api\"\n")
	_, _ = b.WriteString("backup_file \"$DEFAULTS_DIR/xrunner-worker\" \"etc/default/xrunner-worker\"\n")
	_, _ = b.WriteString("backup_file \"$DEFAULTS_DIR/xrunner-remote\" \"etc/default/xrunner-remote\"\n")
	_, _ = b.WriteString("backup_file \"$SYSTEMD_DIR/xrunner-api.service\" \"etc/systemd/system/xrunner-api.service\"\n")
	_, _ = b.WriteString("backup_file \"$SYSTEMD_DIR/xrunner-worker.service\" \"etc/systemd/system/xrunner-worker.service\"\n")
	_, _ = b.WriteString("backup_file \"$SYSTEMD_DIR/xrunner-remote.service\" \"etc/systemd/system/xrunner-remote.service\"\n")

	_, _ = b.WriteString("ensure_port_free_for_xrunner_remote 7337 || exit 1\n")

	_, _ = b.WriteString("if ! getent group \"$XRUNNER_GROUP\" >/dev/null 2>&1; then groupadd --system \"$XRUNNER_GROUP\"; fi\n")
	_, _ = b.WriteString("if ! id -u \"$XRUNNER_USER\" >/dev/null 2>&1; then useradd --system --create-home --home-dir \"$STATE_DIR\" --shell /usr/sbin/nologin --gid \"$XRUNNER_GROUP\" \"$XRUNNER_USER\"; fi\n")
	_, _ = b.WriteString("install -d -m 0755 -o \"$XRUNNER_USER\" -g \"$XRUNNER_GROUP\" \"$STATE_DIR\" \"$STATE_DIR/workspaces\" \"$STATE_DIR/sessions\"\n")
	_, _ = b.WriteString("install -d -m 0755 \"$CONFIG_DIR\"\n")
	_, _ = b.WriteString("install -m 0755 \"$SRC/bin/xrunner-api\" \"$BIN_API\"\n")
	_, _ = b.WriteString("install -m 0755 \"$SRC/bin/xrunner-worker\" \"$BIN_WORKER\"\n")
	_, _ = b.WriteString("install -m 0755 \"$SRC/bin/xrunner-remote\" \"$BIN_REMOTE\"\n")
	_, _ = b.WriteString("if ! \"$BIN_API\" -h >/dev/null 2>&1; then echo \"[xrunner] xrunner-api is not executable\" >&2; ls -la \"$BIN_API\" >&2 || true; file \"$BIN_API\" >&2 || true; exit 1; fi\n")
	_, _ = b.WriteString("if ! \"$BIN_WORKER\" -h >/dev/null 2>&1; then echo \"[xrunner] xrunner-worker is not executable\" >&2; ls -la \"$BIN_WORKER\" >&2 || true; file \"$BIN_WORKER\" >&2 || true; exit 1; fi\n")
	_, _ = b.WriteString("if ! \"$BIN_REMOTE\" -h >/dev/null 2>&1; then echo \"[xrunner] xrunner-remote is not executable\" >&2; ls -la \"$BIN_REMOTE\" >&2 || true; file \"$BIN_REMOTE\" >&2 || true; exit 1; fi\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/configs/default.nsjail.cfg\" \"$CONFIG_DIR/default.nsjail.cfg\"\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/systemd/xrunner-api.env\" \"$DEFAULTS_DIR/xrunner-api\"\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/systemd/xrunner-worker.env\" \"$DEFAULTS_DIR/xrunner-worker\"\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/systemd/xrunner-remote.env\" \"$DEFAULTS_DIR/xrunner-remote\"\n")
	if len(llmEnv) > 0 {
		_, _ = b.WriteString("echo \"[xrunner] configuring LLM backend for xrunner-remote\" >&2\n")
		_, _ = b.WriteString("tmp=$(mktemp)\n")
		_, _ = b.WriteString("grep -v -E '^(XR_LLM_|OPENAI_)' \"$DEFAULTS_DIR/xrunner-remote\" >\"$tmp\" 2>/dev/null || true\n")
		_, _ = b.WriteString("cat >>\"$tmp\" <<'XRUNNER_LLM_ENV'\n")
		for _, l := range llmEnv {
			_, _ = b.WriteString(l)
			_, _ = b.WriteString("\n")
		}
		_, _ = b.WriteString("XRUNNER_LLM_ENV\n")
		_, _ = b.WriteString("install -m 0600 -o root -g root \"$tmp\" \"$DEFAULTS_DIR/xrunner-remote\"\n")
		_, _ = b.WriteString("rm -f \"$tmp\"\n")
	}
	_, _ = b.WriteString("install -m 0644 \"$SRC/systemd/xrunner-api.service\" \"$SYSTEMD_DIR/xrunner-api.service\"\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/systemd/xrunner-worker.service\" \"$SYSTEMD_DIR/xrunner-worker.service\"\n")
	_, _ = b.WriteString("install -m 0644 \"$SRC/systemd/xrunner-remote.service\" \"$SYSTEMD_DIR/xrunner-remote.service\"\n")
	_, _ = b.WriteString("systemctl daemon-reload\n")
	_, _ = b.WriteString("systemctl enable xrunner-worker.service xrunner-api.service xrunner-remote.service\n")
	_, _ = b.WriteString("systemctl restart xrunner-worker.service xrunner-api.service xrunner-remote.service\n")

	_, _ = b.WriteString("check_unit() { systemctl is-active --quiet \"$1\"; }\n")
	_, _ = b.WriteString("check_port() { port=\"$1\"; if command -v ss >/dev/null 2>&1; then ss -ltn 2>/dev/null | grep -q \":${port} \"; else return 0; fi }\n")
	_, _ = b.WriteString("wait_port() {\n")
	_, _ = b.WriteString("  port=\"$1\"\n")
	_, _ = b.WriteString("  i=0\n")
	_, _ = b.WriteString("  until check_port \"$port\"; do\n")
	_, _ = b.WriteString("    i=$((i+1))\n")
	_, _ = b.WriteString("    [ $i -ge 50 ] && return 1\n")
	_, _ = b.WriteString("    sleep 0.2\n")
	_, _ = b.WriteString("  done\n")
	_, _ = b.WriteString("  return 0\n")
	_, _ = b.WriteString("}\n")
	_, _ = b.WriteString("fail() {\n")
	_, _ = b.WriteString("  echo \"[xrunner] bootstrap FAILED\" >&2\n")
	_, _ = b.WriteString("  if command -v ss >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("    echo \"[xrunner] listeners (50051/50052/7337)\" >&2\n")
	_, _ = b.WriteString("    ss -ltnp 2>/dev/null | grep -E ':(50051|50052|7337)[[:space:]]' >&2 || true\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  for u in xrunner-worker.service xrunner-api.service xrunner-remote.service; do\n")
	_, _ = b.WriteString("    a=$(systemctl is-active \"$u\" 2>/dev/null || true)\n")
	_, _ = b.WriteString("    echo \"[xrunner] $u: $a\" >&2\n")
	_, _ = b.WriteString("  done\n")
	_, _ = b.WriteString("  for u in xrunner-worker.service xrunner-api.service xrunner-remote.service; do\n")
	_, _ = b.WriteString("    systemctl status \"$u\" --no-pager -n 80 >&2 || true\n")
	_, _ = b.WriteString("  done\n")
	_, _ = b.WriteString("  if command -v journalctl >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("    journalctl -u xrunner-worker.service -u xrunner-api.service -u xrunner-remote.service --since \"$BOOTSTRAP_START\" --no-pager -n 120 >&2 || true\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  if [ -f \"$REMOTE_LOG\" ]; then\n")
	_, _ = b.WriteString("    mtime=$(stat -c %Y \"$REMOTE_LOG\" 2>/dev/null || echo 0)\n")
	_, _ = b.WriteString("    if [ \"$mtime\" -ge \"$BOOTSTRAP_EPOCH\" ]; then\n")
	_, _ = b.WriteString("      echo \"[xrunner] $REMOTE_LOG (tail)\" >&2\n")
	_, _ = b.WriteString("      tail -n 120 \"$REMOTE_LOG\" >&2 || true\n")
	_, _ = b.WriteString("    else\n")
	_, _ = b.WriteString("      echo \"[xrunner] $REMOTE_LOG exists but looks stale; ignoring (mtime=$mtime)\" >&2\n")
	_, _ = b.WriteString("    fi\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  rollback || true\n")
	_, _ = b.WriteString("  exit 1\n")
	_, _ = b.WriteString("}\n")

	_, _ = b.WriteString("for u in xrunner-worker.service xrunner-api.service xrunner-remote.service; do\n")
	_, _ = b.WriteString("  i=0\n")
	_, _ = b.WriteString("  until check_unit \"$u\"; do\n")
	_, _ = b.WriteString("    i=$((i+1))\n")
	_, _ = b.WriteString("    [ $i -ge 25 ] && fail\n")
	_, _ = b.WriteString("    sleep 0.2\n")
	_, _ = b.WriteString("  done\n")
	_, _ = b.WriteString("done\n")

	// Defaults are from deploy/systemd/*.env; wait for ports to avoid false failures on slow starts.
	_, _ = b.WriteString("wait_port 50052 || fail\n")
	_, _ = b.WriteString("wait_port 50051 || fail\n")
	_, _ = b.WriteString("wait_port 7337 || fail\n")

	_, _ = b.WriteString("echo \"[xrunner] bootstrap ok\" \n")
	_, _ = b.WriteString("echo \"[xrunner] services: worker=active api=active remote=active\" \n")
	_, _ = b.WriteString("echo \"[xrunner] ports: 50052(worker) 50051(api) 7337(remote)\" \n")
	return b.Bytes(), nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
