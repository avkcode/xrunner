package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	cliconfig "github.com/antonkrylov/xrunner/internal/cli/config"
)

func newSSHCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "SSH helpers for xrunner-remote (PTY, sessions, events)",
	}
	cmd.AddCommand(newSSHBootstrapCmd())
	cmd.AddCommand(newSSHUninstallCmd())
	cmd.AddCommand(newSSHProxyCmd())
	cmd.AddCommand(newSSHAttachCmd())
	cmd.AddCommand(newSSHChatCmd())
	cmd.AddCommand(newSSHExecCmd())
	cmd.AddCommand(newSSHSessionCmd())
	cmd.AddCommand(newSSHShellCmd())
	return cmd
}

func newSSHShellCmd() *cobra.Command {
	var flags sshRemoteFlags
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Durable shell helpers (timeline, search)",
	}
	flags.bind(cmd)
	cmd.AddCommand(newSSHShellTimelineCmd(&flags))
	cmd.AddCommand(newSSHShellSearchCmd(&flags))
	cmd.AddCommand(newSSHShellArtifactsCmd(&flags))
	return cmd
}

func newSSHShellTimelineCmd(flags *sshRemoteFlags) *cobra.Command {
	var name string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "timeline",
		Short: "List RunShell commands recorded for a durable shell session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}

			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if limit <= 0 {
				limit = 50
			}

			shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
			resp, err := shellc.ListShellCommands(ctx, &xrunnerv1.ListShellCommandsRequest{Name: name, Limit: uint32(limit)})
			if err != nil {
				return err
			}
			if asJSON {
				b, err := json.Marshal(resp.GetCommands())
				if err != nil {
					return err
				}
				_, _ = os.Stdout.Write(append(b, '\n'))
				return nil
			}
			for _, r := range resp.GetCommands() {
				if r == nil {
					continue
				}
				start := ""
				if r.GetStartedAt() != nil {
					start = r.GetStartedAt().AsTime().Format(time.RFC3339)
				}
				dur := ""
				if r.GetStartedAt() != nil && r.GetCompletedAt() != nil {
					dur = r.GetCompletedAt().AsTime().Sub(r.GetStartedAt().AsTime()).Truncate(time.Millisecond).String()
				}
				fmt.Fprintf(os.Stdout, "%s exit=%d dur=%s bytes=%d id=%s cmd=%s\n",
					start,
					r.GetExitCode(),
					dur,
					r.GetOutputBytes(),
					r.GetCommandId(),
					r.GetCommand(),
				)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "durable shell session name")
	cmd.Flags().IntVar(&limit, "limit", 50, "max records to return")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newSSHShellSearchCmd(flags *sshRemoteFlags) *cobra.Command {
	var name string
	var query string
	var commandID string
	var since string
	var until string
	var maxMatches int
	var contextBytes int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search durable shell log (optionally constrained by RunShell timeline)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}

			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if strings.TrimSpace(query) == "" {
				return fmt.Errorf("--query is required")
			}
			var startTS *timestamppb.Timestamp
			var endTS *timestamppb.Timestamp
			if strings.TrimSpace(since) != "" {
				tm, err := time.Parse(time.RFC3339, strings.TrimSpace(since))
				if err != nil {
					return fmt.Errorf("invalid --since (use RFC3339): %w", err)
				}
				startTS = timestamppb.New(tm)
			}
			if strings.TrimSpace(until) != "" {
				tm, err := time.Parse(time.RFC3339, strings.TrimSpace(until))
				if err != nil {
					return fmt.Errorf("invalid --until (use RFC3339): %w", err)
				}
				endTS = timestamppb.New(tm)
			}
			if maxMatches <= 0 {
				maxMatches = 50
			}
			if contextBytes <= 0 {
				contextBytes = 80
			}

			shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
			resp, err := shellc.SearchShell(ctx, &xrunnerv1.ShellSearchRequest{
				Name:         name,
				Query:        []byte(query),
				CommandId:    strings.TrimSpace(commandID),
				StartTime:    startTS,
				EndTime:      endTS,
				MaxMatches:   uint32(maxMatches),
				ContextBytes: uint32(contextBytes),
			})
			if err != nil {
				return err
			}
			if asJSON {
				b, err := json.Marshal(resp)
				if err != nil {
					return err
				}
				_, _ = os.Stdout.Write(append(b, '\n'))
				return nil
			}
			for _, m := range resp.GetMatches() {
				if m == nil {
					continue
				}
				cid := strings.TrimSpace(m.GetCommandId())
				if cid != "" {
					fmt.Fprintf(os.Stdout, "off=%d cmd=%s %q\n", m.GetOffset(), cid, string(m.GetSnippet()))
				} else {
					fmt.Fprintf(os.Stdout, "off=%d %q\n", m.GetOffset(), string(m.GetSnippet()))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "durable shell session name")
	cmd.Flags().StringVar(&query, "query", "", "search query (substring)")
	cmd.Flags().StringVar(&commandID, "command-id", "", "restrict to a specific RunShell command_id")
	cmd.Flags().StringVar(&since, "since", "", "restrict by started_at >= RFC3339")
	cmd.Flags().StringVar(&until, "until", "", "restrict by started_at <= RFC3339")
	cmd.Flags().IntVar(&maxMatches, "max", 50, "max matches")
	cmd.Flags().IntVar(&contextBytes, "context", 80, "context bytes around match")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newSSHShellArtifactsCmd(flags *sshRemoteFlags) *cobra.Command {
	var name string
	var commandID string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "artifacts",
		Short: "List structured artifacts extracted for shell commands",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}

			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if limit <= 0 {
				limit = 50
			}
			shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
			resp, err := shellc.ListShellArtifacts(ctx, &xrunnerv1.ListShellArtifactsRequest{
				Name:      name,
				CommandId: strings.TrimSpace(commandID),
				Limit:     uint32(limit),
			})
			if err != nil {
				return err
			}
			if asJSON {
				b, err := json.Marshal(resp)
				if err != nil {
					return err
				}
				_, _ = os.Stdout.Write(append(b, '\n'))
				return nil
			}
			for _, a := range resp.GetArtifacts() {
				if a == nil {
					continue
				}
				fmt.Fprintf(os.Stdout, "id=%s type=%s cmd=%s title=%s\n", a.GetId(), a.GetType(), a.GetCommandId(), a.GetTitle())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "durable shell session name")
	cmd.Flags().StringVar(&commandID, "command-id", "", "filter by command id")
	cmd.Flags().IntVar(&limit, "limit", 50, "max artifacts to return")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

type sshRemoteFlags struct {
	proxyAddr   string
	host        string
	remotePort  int
	remoteBin   string
	startRemote bool
	installFrom string
	batchMode   bool
	upstream    string
	upstreamTLS bool
	sessionsDir string
}

func (f *sshRemoteFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.proxyAddr, "proxy", "", "reuse an existing local proxy address (or set XRUNNER_SSH_PROXY)")
	cmd.Flags().StringVar(&f.host, "host", "", "SSH target (e.g. root@188.124.37.233)")
	cmd.Flags().IntVar(&f.remotePort, "remote-port", 7337, "xrunner-remote listen port on remote localhost")
	cmd.Flags().StringVar(&f.remoteBin, "remote-bin", "~/.xrunner/bin/xrunner-remote", "path to xrunner-remote on the remote host")
	cmd.Flags().BoolVar(&f.startRemote, "start-remote", true, "attempt to start xrunner-remote via ssh if ping fails")
	cmd.Flags().StringVar(&f.installFrom, "install-from", "", "local path to xrunner-remote to scp if missing")
	cmd.Flags().BoolVar(&f.batchMode, "batch", false, "set ssh BatchMode=yes (fail instead of prompting for passwords)")
	cmd.Flags().StringVar(&f.upstream, "upstream", "127.0.0.1:50051", "upstream JobService address (when starting remote daemon)")
	cmd.Flags().BoolVar(&f.upstreamTLS, "upstream-tls", false, "use TLS when dialing upstream JobService (when starting remote daemon)")
	cmd.Flags().StringVar(&f.sessionsDir, "sessions-dir", "", "remote sessions dir passed to xrunner-remote (default ~/.xrunner/sessions)")
}

type sshTunnel struct {
	localAddr string
	proc      *exec.Cmd
}

func (t *sshTunnel) Close() {
	if t == nil || t.proc == nil || t.proc.Process == nil {
		return
	}
	_ = t.proc.Process.Kill()
	_, _ = t.proc.Process.Wait()
}

func startTunnel(ctx context.Context, host string, remotePort int, batchMode bool) (*sshTunnel, error) {
	localPort, err := freeLocalPort()
	if err != nil {
		return nil, err
	}
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	args := []string{
		"-o", "ExitOnForwardFailure=yes",
		// Reuse TCP connections when possible to reduce handshake flakiness.
		// %C is a hash of connection params (user/host/port).
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=60s",
		"-o", "ControlPath=~/.ssh/xrunner-%C",
		// Keep tunnels stable across NATs and stricter SSH servers.
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
		// Avoid hanging too long on bad networks/DNS.
		"-o", "ConnectTimeout=10",
	}
	if batchMode {
		args = append(args, "-o", "BatchMode=yes")
	}
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
	t := &sshTunnel{localAddr: localAddr, proc: fwd}
	// Don't probe the forwarded port here; if the remote daemon isn't up yet,
	// connecting triggers noisy "channel open failed" messages from ssh.
	return t, nil
}

func dialRemote(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	for {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := grpc.DialContext(
			dctx,
			addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
			grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				// Be conservative: some deployments keep the default server enforcement policy
				// (MinTime=5m), which will send GOAWAY "too_many_pings" for more aggressive settings.
				Time:                5 * time.Minute,
				Timeout:             20 * time.Second,
				PermitWithoutStream: false,
			}),
		)
		cancel()
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (f sshRemoteFlags) resolvedProxyAddr() string {
	if v := strings.TrimSpace(f.proxyAddr); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("XRUNNER_SSH_PROXY"))
}

func (f *sshRemoteFlags) applyDefaultsFromConfig(cmd *cobra.Command) {
	if f == nil {
		return
	}
	if strings.TrimSpace(f.host) != "" || f.resolvedProxyAddr() != "" {
		return
	}

	cfgPath := ""
	if cmd != nil && cmd.Root() != nil {
		if v, err := cmd.Root().PersistentFlags().GetString("config"); err == nil {
			cfgPath = v
		}
	}
	if strings.TrimSpace(cfgPath) == "" {
		if v := strings.TrimSpace(os.Getenv("XRUNNER_CONFIG")); v != "" {
			cfgPath = v
		} else {
			cfgPath = cliconfig.DefaultConfigPath()
		}
	}

	ctxName := ""
	if cmd != nil && cmd.Root() != nil {
		if v, err := cmd.Root().PersistentFlags().GetString("context"); err == nil {
			ctxName = v
		}
	}

	cfg, err := cliconfig.Load(cfgPath)
	if err != nil || cfg == nil {
		return
	}
	ctx, _, err := cfg.Resolve(ctxName)
	if err != nil || ctx == nil {
		return
	}
	if v := strings.TrimSpace(ctx.SSHHost); v != "" {
		f.host = v
	}
}

func (f sshRemoteFlags) validateHostOrProxy(cmd *cobra.Command) error {
	if f.resolvedProxyAddr() != "" {
		return nil
	}
	if strings.TrimSpace(f.host) == "" {
		ff := f
		(&ff).applyDefaultsFromConfig(cmd)
		if strings.TrimSpace(ff.host) == "" {
			return fmt.Errorf("--host is required (or set --proxy / XRUNNER_SSH_PROXY). Tip: `xrunner ssh bootstrap --write-config ...` to save sshHost in $HOME/.xrunner/config")
		}
	}
	return nil
}

func dialRemoteOnce(ctx context.Context, addr string, timeout time.Duration) (*grpc.ClientConn, error) {
	if timeout <= 0 {
		timeout = 1200 * time.Millisecond
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return grpc.DialContext(
		dctx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// Be conservative: some deployments keep the default server enforcement policy
			// (MinTime=5m), which will send GOAWAY "too_many_pings" for more aggressive settings.
			Time:                5 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
	)
}

func (f sshRemoteFlags) connect(ctx context.Context, cmd *cobra.Command) (*grpc.ClientConn, func(), error) {
	if addr := f.resolvedProxyAddr(); addr != "" {
		conn, err := dialRemote(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
	if strings.TrimSpace(f.host) == "" {
		// Convenience for local dev: if a local xrunner-remote is already running, use it.
		if conn, err := dialRemoteOnce(ctx, "127.0.0.1:7337", 800*time.Millisecond); err == nil {
			return conn, func() { _ = conn.Close() }, nil
		}
		ff := f
		(&ff).applyDefaultsFromConfig(cmd)
		f = ff
		return nil, nil, f.validateHostOrProxy(cmd)
	}

	// Fast path: establish an SSH tunnel and try to dial/ping the remote daemon without running
	// any remote shell commands. This is important for systemd-managed deployments where
	// xrunner-remote is already running and SSH command execution may be restricted/flaky.
	tunnel, err := startTunnel(ctx, f.host, f.remotePort, f.batchMode)
	if err != nil {
		return nil, nil, err
	}
	lastErr := error(nil)
	// SSH handshake + port forward setup can take a few seconds on real networks;
	// give it a little headroom to avoid flaky "deadline exceeded" failures.
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := waitForTCP(waitCtx, tunnel.localAddr); err != nil {
		lastErr = err
	}
	waitCancel()
	conn, err := dialRemoteOnce(ctx, tunnel.localAddr, 4*time.Second)
	if err == nil {
		pingErr := pingRemote(ctx, conn, f.host)
		if pingErr == nil {
			return conn, func() {
				_ = conn.Close()
				tunnel.Close()
			}, nil
		}
		lastErr = pingErr
		_ = conn.Close()
	} else {
		lastErr = err
	}
	tunnel.Close()

	// If we aren't allowed to start the remote daemon, fail with guidance.
	if !f.startRemote {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("remote daemon is not reachable on localhost:%d; rerun with --start-remote or start it manually: %w", f.remotePort, lastErr)
		}
		return nil, nil, fmt.Errorf("remote daemon is not reachable on localhost:%d; rerun with --start-remote or start it manually", f.remotePort)
	}

	// Slow path: bootstrap xrunner-remote via ssh, then reconnect.
	if err := ensureRemoteDaemonViaSSH(ctx, f); err != nil {
		return nil, nil, err
	}
	tunnel, err = startTunnel(ctx, f.host, f.remotePort, f.batchMode)
	if err != nil {
		return nil, nil, err
	}
	conn, err = dialRemote(ctx, tunnel.localAddr)
	if err != nil {
		tunnel.Close()
		return nil, nil, err
	}
	return conn, func() {
		_ = conn.Close()
		tunnel.Close()
	}, nil
}

func ensureRemoteDaemonViaSSH(ctx context.Context, flags sshRemoteFlags) error {
	if !flags.startRemote {
		return fmt.Errorf("remote daemon is not reachable on localhost:%d; rerun with --start-remote or start it manually", flags.remotePort)
	}

	sshBase := []string{}
	if flags.batchMode {
		sshBase = append(sshBase, "-o", "BatchMode=yes")
	}

	remoteBin := flags.remoteBin
	if strings.HasPrefix(remoteBin, "~") {
		remoteBin = strings.Replace(remoteBin, "~", "$HOME", 1)
	}

	// Optional install.
	if flags.installFrom != "" {
		if _, err := os.Stat(flags.installFrom); err != nil {
			return fmt.Errorf("install-from: %w", err)
		}
		if err := runLocal(ctx, "ssh", append(append([]string(nil), sshBase...), flags.host, "mkdir -p ~/.xrunner/bin")...); err != nil {
			return err
		}
		scpArgs := append([]string(nil), sshBase...)
		scpArgs = append(scpArgs, flags.installFrom, fmt.Sprintf("%s:~/.xrunner/bin/xrunner-remote", flags.host))
		if err := runLocal(ctx, "scp", scpArgs...); err != nil {
			return err
		}
		if err := runLocal(ctx, "ssh", append(append([]string(nil), sshBase...), flags.host, "chmod +x ~/.xrunner/bin/xrunner-remote")...); err != nil {
			return err
		}
	}

	if err := runLocal(ctx, "ssh", append(append([]string(nil), sshBase...), flags.host, "mkdir -p ~/.xrunner")...); err != nil {
		return err
	}

	// If the binary doesn't exist and we weren't asked to install it, fail early with guidance.
	checkBin := fmt.Sprintf("test -x %s", remoteBin)
	if err := runLocal(ctx, "ssh", append(append([]string(nil), sshBase...), flags.host, checkBin)...); err != nil {
		// If a bootstrap-style installation exists, fall back to /usr/local/bin.
		if err2 := runLocal(ctx, "ssh", append(append([]string(nil), sshBase...), flags.host, "test -x /usr/local/bin/xrunner-remote")...); err2 == nil {
			remoteBin = "/usr/local/bin/xrunner-remote"
			checkBin = fmt.Sprintf("test -x %s", remoteBin)
		} else {
			return fmt.Errorf("xrunner-remote missing on %s at %s (use --install-from to scp a Linux binary)", flags.host, flags.remoteBin)
		}
	}

	args := []string{
		"nohup", remoteBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", flags.remotePort),
		"--upstream", flags.upstream,
		"--workspace-root", "/",
		"--unsafe-allow-rootfs=true",
	}
	if flags.upstreamTLS {
		args = append(args, "--upstream-tls=true")
	}
	if strings.TrimSpace(flags.sessionsDir) != "" {
		args = append(args, "--sessions-dir", flags.sessionsDir)
	}

	// Forward local LLM env so `xrunner ssh ...` can start a functional remote daemon without
	// requiring remote-side shell profiles or systemd env files.
	//
	// Note: This is intentionally opt-in (only forwards vars that are set locally).
	forwardedEnv := func() string {
		keys := []string{
			// Primary configuration.
			"XR_LLM_ENABLED",
			"XR_LLM_PROVIDER",
			"XR_LLM_BASE_URL",
			"XR_LLM_API_KEY",
			"XR_LLM_MODEL",
			"XR_LLM_TIMEOUT_MS",
			"XR_LLM_STREAM",
			"XR_LLM_MAX_TOOL_ITERS",
			"XR_LLM_TOOL_OUTPUT_MAX_BYTES",
			"XR_LLM_MAX_HISTORY_EVENTS",
			"XR_LLM_SYSTEM",
			// OpenAI-compatible knobs.
			"XR_LLM_CHAT_PATH",
			"XR_LLM_API_KEY_HEADER",
			"XR_LLM_API_KEY_PREFIX",
			// Gemini-specific knobs (even if tool calling isn't supported yet).
			"XR_LLM_GEMINI_KEY_HEADER",
			"XR_LLM_GEMINI_OPERATOR",
			// Fallbacks.
			"OPENAI_BASE_URL",
			"OPENAI_API_KEY",
			"OPENAI_MODEL",
		}
		var b strings.Builder
		for _, k := range keys {
			v := strings.TrimSpace(os.Getenv(k))
			if v == "" {
				continue
			}
			_, _ = b.WriteString("export ")
			_, _ = b.WriteString(k)
			_, _ = b.WriteString("=")
			_, _ = b.WriteString(shQuote(v))
			_, _ = b.WriteString("\n")
		}
		return b.String()
	}()

	// Start daemon and verify it actually bound the port. `nohup ... &` can return
	// success even when the binary fails immediately, so we probe localhost.
	startScript := forwardedEnv + strings.Join(args, " ") + " >~/.xrunner/remote.log 2>&1 &\n" +
		fmt.Sprintf("for i in $(seq 1 25); do (echo > /dev/tcp/127.0.0.1/%d) >/dev/null 2>&1 && exit 0; sleep 0.2; done\n", flags.remotePort) +
		"echo \"[xrunner] xrunner-remote did not start; tailing ~/.xrunner/remote.log\" >&2\n" +
		"tail -n 120 ~/.xrunner/remote.log >&2 || true\n" +
		"exit 1\n"
	startCmd := "bash -lc " + shQuote(startScript)

	if err := runLocal(ctx, "ssh", append(append([]string(nil), sshBase...), flags.host, startCmd)...); err != nil {
		return err
	}

	return nil
}

func pingRemote(ctx context.Context, conn *grpc.ClientConn, host string) error {
	ctrl := xrunnerv1.NewRemoteControlServiceClient(conn)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := ctrl.Ping(ctx, &emptypb.Empty{}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("remote daemon did not become ready; check ~/.xrunner/remote.log on %s", host)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func replayTmux(ctx context.Context, conn *grpc.ClientConn, name string, lines int, asJSON bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("resume name is required for replay")
	}
	if lines <= 0 {
		return fmt.Errorf("replay lines must be > 0")
	}

	shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Ensure the session exists so replay has a stable log source.
	_, _ = shellc.EnsureShellSession(rctx, &xrunnerv1.EnsureShellSessionRequest{Name: name})
	tail, err := shellc.TailShell(rctx, &xrunnerv1.ShellTailRequest{Name: name, Lines: uint32(lines), MaxBytes: 4 * 1024 * 1024})
	if err != nil {
		return err
	}
	out := strings.ReplaceAll(string(tail.GetData()), "\r\n", "\n")
	if asJSON {
		b, err := json.Marshal(struct {
			Session string `json:"session"`
			Lines   int    `json:"lines"`
			Text    string `json:"text"`
		}{Session: name, Lines: lines, Text: out})
		if err != nil {
			return err
		}
		_, _ = os.Stdout.Write(append(b, '\n'))
		return nil
	}
	_, _ = io.WriteString(os.Stdout, out)
	return nil
}

func newSSHAttachCmd() *cobra.Command {
	var flags sshRemoteFlags
	var cmdStr string
	var args []string
	var cwd string
	var envItems []string
	var ptySession string
	var termEnv string
	var statusInterval time.Duration
	var resume string
	var replayLines int
	var replayOnly bool
	var replayJSON bool
	var shellCompress string
	var commandMode bool

	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attach to a remote PTY via xrunner-remote (interactive)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}

			if r := strings.TrimSpace(resume); r != "" {
				shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
				cols, rows := termSize()
				ens, err := shellc.EnsureShellSession(ctx, &xrunnerv1.EnsureShellSessionRequest{
					Name: r,
					Cols: uint32(cols),
					Rows: uint32(rows),
				})
				if err != nil {
					return err
				}

				startOffset := ens.GetLogSize()
				if replayLines > 0 {
					rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
					tail, err := shellc.TailShell(rctx, &xrunnerv1.ShellTailRequest{Name: r, Lines: uint32(replayLines), MaxBytes: 4 * 1024 * 1024})
					cancel()
					if err != nil {
						if replayOnly {
							return err
						}
						fmt.Fprintf(os.Stderr, "replay failed: %v\n", err)
					} else {
						out := strings.ReplaceAll(string(tail.GetData()), "\r\n", "\n")
						if replayJSON {
							b, err := json.Marshal(struct {
								Session string `json:"session"`
								Lines   int    `json:"lines"`
								Text    string `json:"text"`
							}{Session: r, Lines: replayLines, Text: out})
							if err == nil {
								_, _ = os.Stdout.Write(append(b, '\n'))
							}
						} else {
							_, _ = io.WriteString(os.Stdout, out)
						}
						startOffset = tail.GetLogSize()
					}
					if replayOnly {
						return nil
					}
				}

				comp := xrunnerv1.ShellClientHello_COMPRESSION_NONE
				switch strings.ToLower(strings.TrimSpace(shellCompress)) {
				case "", "none", "off":
					comp = xrunnerv1.ShellClientHello_COMPRESSION_NONE
				case "zstd":
					comp = xrunnerv1.ShellClientHello_COMPRESSION_ZSTD
				default:
					return fmt.Errorf("invalid --shell-compress %q (use none|zstd)", shellCompress)
				}

				if commandMode {
					return runShellCommandMode(ctx, shellc, r, startOffset, comp)
				}

				restore, err := makeStdinRaw()
				if err != nil {
					return err
				}
				defer restore()

				started := time.Now()
				bw := newBatchWriter(os.Stdout, 16*time.Millisecond, 256*1024)
				if err := pumpShell(ctx, shellc, r, startOffset, bw, statusInterval, comp); err != nil {
					bw.Close()
					return err
				}
				bw.Close()
				if statusInterval > 0 {
					fmt.Fprintf(os.Stderr, "\nshell disconnected name=%s dur=%s bytes=%d\n", r, time.Since(started).Truncate(time.Millisecond), bw.TotalBytes())
				}
				return nil
			}

			ptyc := xrunnerv1.NewRemotePTYServiceClient(conn)
			sid := strings.TrimSpace(ptySession)
			allowNotFound := false
			if sid == "" {
				env, err := parseKeyValue(envItems)
				if err != nil {
					return err
				}
				applyTerminalDefaults(env, termEnv)
				if r := strings.TrimSpace(resume); r != "" {
					cmdStr = "sh"
					args = []string{"-lc", fmt.Sprintf("exec tmux new -As %s", shellEscapeArg(r))}
				}
				cols, rows := termSize()
				open, err := ptyc.OpenPTY(ctx, &xrunnerv1.OpenPTYRequest{
					Command: cmdStr,
					Args:    args,
					Cwd:     cwd,
					Env:     env,
					Cols:    uint32(cols),
					Rows:    uint32(rows),
				})
				if err != nil {
					return err
				}
				sid = open.GetSessionId()
				allowNotFound = true
				fmt.Fprintf(os.Stderr, "pty session: %s\n", sid)
				defer func() { _, _ = ptyc.ClosePTY(context.Background(), &xrunnerv1.PTYCloseRequest{SessionId: sid}) }()
				_, _ = ptyc.ResizePTY(context.Background(), &xrunnerv1.PTYResizeRequest{SessionId: sid, Cols: uint32(cols), Rows: uint32(rows)})
			}

			restore, err := makeStdinRaw()
			if err != nil {
				return err
			}
			defer restore()

			started := time.Now()
			bw := newBatchWriter(os.Stdout, 16*time.Millisecond, 256*1024)
			if err := pumpPTY(ctx, ptyc, sid, allowNotFound, bw, statusInterval); err != nil {
				bw.Close()
				return err
			}
			bw.Close()
			if statusInterval > 0 {
				fmt.Fprintf(os.Stderr, "\npty disconnected dur=%s bytes=%d\n", time.Since(started).Truncate(time.Millisecond), bw.TotalBytes())
			}
			return nil
		},
	}

	flags.bind(cmd)
	cmd.Flags().StringVar(&ptySession, "pty-session", "", "existing PTY session id to reattach to")
	cmd.Flags().StringVar(&cmdStr, "cmd", "sh", "command to run in the PTY")
	cmd.Flags().StringArrayVar(&args, "arg", nil, "command argument (repeatable)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "remote working directory")
	cmd.Flags().StringArrayVar(&envItems, "env", nil, "remote environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringVar(&termEnv, "term", "", "TERM value injected for PTY sessions (defaults to local TERM or xterm-256color)")
	cmd.Flags().DurationVar(&statusInterval, "status-interval", 1*time.Second, "print a one-line status to stderr at this interval (0 disables)")
	cmd.Flags().StringVar(&resume, "resume", "", "attach to a durable tmux session name (runs `tmux new -As <name>`)")
	cmd.Flags().IntVar(&replayLines, "replay", 0, "when used with --resume, print last N lines of tmux history before attaching")
	cmd.Flags().BoolVar(&replayOnly, "replay-only", false, "only replay tmux history and exit (requires --resume and --replay)")
	cmd.Flags().BoolVar(&replayJSON, "replay-json", false, "emit tmux replay as JSON to stdout (requires --resume and --replay)")
	cmd.Flags().StringVar(&shellCompress, "shell-compress", "none", "compress durable shell stream: none|zstd (only with --resume)")
	cmd.Flags().BoolVar(&commandMode, "command-mode", false, "send one-line commands as structured runs (works best with --resume; not for full-screen TUIs)")
	return cmd
}

func pumpShell(ctx context.Context, shellc xrunnerv1.RemoteShellServiceClient, name string, startOffset uint64, out io.Writer, statusInterval time.Duration, compression xrunnerv1.ShellClientHello_Compression) error {
	stream, err := shellc.AttachShell(ctx)
	if err != nil {
		return err
	}

	hn, _ := os.Hostname()
	hn = strings.TrimSpace(hn)
	clientID := "cli:" + hn + ":" + name
	if hn == "" {
		clientID = "cli:" + name
	}

	sendCh := make(chan *xrunnerv1.ShellClientMsg, 128)
	sendErr := make(chan error, 1)
	done := make(chan struct{})
	enqueue := func(msg *xrunnerv1.ShellClientMsg) {
		if msg == nil {
			return
		}
		select {
		case <-done:
			return
		case sendCh <- msg:
		}
	}
	go func() {
		defer close(sendErr)
		for msg := range sendCh {
			if msg == nil {
				continue
			}
			if err := stream.Send(msg); err != nil {
				sendErr <- err
				return
			}
		}
	}()

	enqueue(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
		Name:              name,
		AfterOffset:       startOffset,
		ClientId:          clientID,
		ResumeFromLastAck: false,
		MaxChunkBytes:     0, // allow server adaptive tuning
		PollMs:            0,
		Compression:       compression,
	}}})

	errCh := make(chan error, 3)
	var received atomic.Uint64
	var dec *zstd.Decoder
	var decOnce sync.Once
	var decErr error

	go func() {
		for {
			srv, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			switch k := srv.GetKind().(type) {
			case *xrunnerv1.ShellServerMsg_Hello:
				if k.Hello != nil && k.Hello.GetCompression() == xrunnerv1.ShellClientHello_COMPRESSION_ZSTD {
					decOnce.Do(func() {
						dec, decErr = zstd.NewReader(nil)
					})
					if decErr != nil {
						errCh <- decErr
						return
					}
				}
			case *xrunnerv1.ShellServerMsg_Chunk:
				if k.Chunk == nil || len(k.Chunk.GetData()) == 0 {
					continue
				}
				data := k.Chunk.GetData()
				uncompressedLen := int(k.Chunk.GetUncompressedLen())
				if uncompressedLen <= 0 {
					uncompressedLen = len(data)
				}
				if dec != nil {
					plain, derr := dec.DecodeAll(data, nil)
					if derr != nil {
						errCh <- derr
						return
					}
					data = plain
				}
				received.Add(uint64(len(data)))
				_, _ = out.Write(data)
				enqueue(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
					Name:      name,
					ClientId:  clientID,
					AckOffset: k.Chunk.GetOffset() + uint64(uncompressedLen),
				}}})
			default:
				// ignore
			}
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				enqueue(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Input{Input: &xrunnerv1.ShellClientInput{
					Name: name,
					Data: append([]byte(nil), buf[:n]...),
				}}})
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					return
				}
				errCh <- rerr
				return
			}
		}
	}()

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			cols, rows := termSize()
			enqueue(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Resize{Resize: &xrunnerv1.ShellClientResize{
				Name: name,
				Cols: uint32(cols),
				Rows: uint32(rows),
			}}})
		}
	}()

	if statusInterval > 0 && term.IsTerminal(int(os.Stderr.Fd())) {
		go func() {
			ticker := time.NewTicker(statusInterval)
			defer ticker.Stop()
			var last uint64
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					now := received.Load()
					delta := now - last
					last = now
					rate := float64(delta) / statusInterval.Seconds()
					fmt.Fprintf(os.Stderr, "\rconnected shell=%s bytes=%d rate=%.0fB/s", name, now, rate)
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
		if dec != nil {
			dec.Close()
		}
		close(done)
		close(sendCh)
		return nil
	case err := <-sendErr:
		if dec != nil {
			dec.Close()
		}
		close(done)
		close(sendCh)
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case err := <-errCh:
		if dec != nil {
			dec.Close()
		}
		close(done)
		close(sendCh)
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

func runShellCommandMode(ctx context.Context, shellc xrunnerv1.RemoteShellServiceClient, name string, startOffset uint64, compression xrunnerv1.ShellClientHello_Compression) error {
	stream, err := shellc.AttachShell(ctx)
	if err != nil {
		return err
	}

	hn, _ := os.Hostname()
	hn = strings.TrimSpace(hn)
	clientID := "cli-cmd:" + hn + ":" + name
	if hn == "" {
		clientID = "cli-cmd:" + name
	}

	if err := stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
		Name:              name,
		AfterOffset:       startOffset,
		ClientId:          clientID,
		ResumeFromLastAck: false,
		MaxChunkBytes:     0,
		PollMs:            0,
		Compression:       compression,
	}}}); err != nil {
		return err
	}

	completedCh := make(chan string, 32)
	var lastAck atomic.Uint64
	var dec *zstd.Decoder
	var decOnce sync.Once
	var decErr error

	recvErrCh := make(chan error, 1)
	go func() {
		defer close(recvErrCh)
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErrCh <- err
				return
			}
			switch k := msg.GetKind().(type) {
			case *xrunnerv1.ShellServerMsg_Hello:
				if k.Hello != nil && k.Hello.GetCompression() == xrunnerv1.ShellClientHello_COMPRESSION_ZSTD {
					decOnce.Do(func() { dec, decErr = zstd.NewReader(nil) })
					if decErr != nil {
						recvErrCh <- decErr
						return
					}
				}
			case *xrunnerv1.ShellServerMsg_Chunk:
				if k.Chunk == nil || len(k.Chunk.GetData()) == 0 {
					continue
				}
				data := k.Chunk.GetData()
				if dec != nil {
					plain, derr := dec.DecodeAll(data, nil)
					if derr != nil {
						recvErrCh <- derr
						return
					}
					data = plain
				}
				_, _ = os.Stdout.Write(data)
				adv := uint64(k.Chunk.GetUncompressedLen())
				if adv == 0 {
					adv = uint64(len(k.Chunk.GetData()))
				}
				ack := k.Chunk.GetOffset() + adv
				lastAck.Store(ack)
				_ = stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
					Name:      name,
					ClientId:  clientID,
					AckOffset: ack,
				}}})
			case *xrunnerv1.ShellServerMsg_Command:
				if k.Command != nil && k.Command.GetPhase() == xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED && k.Command.GetRecord() != nil {
					select {
					case completedCh <- k.Command.GetRecord().GetCommandId():
					default:
					}
				}
			default:
			}
		}
	}()

	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for {
		_, _ = fmt.Fprintf(os.Stderr, "xrunner(%s)> ", name)
		if !sc.Scan() {
			_ = stream.CloseSend()
			select {
			case err := <-recvErrCh:
				if err == nil || errors.Is(err, io.EOF) {
					return nil
				}
				return err
			case <-time.After(250 * time.Millisecond):
				return nil
			}
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			_ = stream.CloseSend()
			return nil
		}
		cmdID := strings.ReplaceAll(uuid.NewString(), "-", "")
		if err := stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
			Name:      name,
			Command:   line,
			CommandId: cmdID,
			TimeoutMs: 0,
		}}}); err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case err := <-recvErrCh:
				if err == nil || errors.Is(err, io.EOF) {
					return nil
				}
				return err
			case id := <-completedCh:
				if id == cmdID {
					// Prompt will be printed on next loop iteration.
					goto next
				}
			}
		}
	next:
		_ = lastAck.Load()
	}
}

func pumpPTY(ctx context.Context, ptyc xrunnerv1.RemotePTYServiceClient, sessionID string, allowNotFound bool, out io.Writer, statusInterval time.Duration) error {
	stream, err := ptyc.StreamPTY(ctx, &xrunnerv1.PTYStreamRequest{SessionId: sessionID})
	if err != nil {
		if allowNotFound && isNotFound(err) {
			return nil
		}
		return err
	}

	errCh := make(chan error, 2)
	var received atomic.Uint64

	go func() {
		for {
			ch, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			if len(ch.GetData()) > 0 {
				received.Add(uint64(len(ch.GetData())))
				_, _ = out.Write(ch.GetData())
			}
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				_, werr := ptyc.WritePTY(ctx, &xrunnerv1.PTYWriteRequest{SessionId: sessionID, Data: append([]byte(nil), buf[:n]...)})
				if werr != nil {
					errCh <- werr
					return
				}
			}
			if rerr != nil {
				// In non-interactive contexts, stdin is often immediately closed.
				// Don't treat that as a PTY failure; keep streaming output.
				if errors.Is(rerr, io.EOF) {
					return
				}
				errCh <- rerr
				return
			}
		}
	}()

	// Best-effort resize loop.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			cols, rows := termSize()
			_, _ = ptyc.ResizePTY(context.Background(), &xrunnerv1.PTYResizeRequest{
				SessionId: sessionID,
				Cols:      uint32(cols),
				Rows:      uint32(rows),
			})
		}
	}()

	if statusInterval > 0 && term.IsTerminal(int(os.Stderr.Fd())) {
		go func() {
			ticker := time.NewTicker(statusInterval)
			defer ticker.Stop()
			var last uint64
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					now := received.Load()
					delta := now - last
					last = now
					rate := float64(delta) / statusInterval.Seconds()
					fmt.Fprintf(os.Stderr, "\rconnected pty=%s bytes=%d rate=%.0fB/s", sessionID, now, rate)
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		// If the remote process exits, gRPC stream usually ends with EOF.
		if errors.Is(err, io.EOF) {
			return nil
		}
		// creack/pty on Linux can surface EIO once the PTY closes; the server maps
		// it to an INTERNAL gRPC error. Treat it as a normal end-of-stream.
		if isPTYClosed(err) {
			return nil
		}
		return err
	}
}

func isPTYClosed(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if st.Code() != codes.Internal {
		return false
	}
	return strings.Contains(strings.ToLower(st.Message()), "input/output error")
}

func isNotFound(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}

func newSSHExecCmd() *cobra.Command {
	var flags sshRemoteFlags
	var sessionID string
	var cmdStr string
	var cwd string
	var envItems []string
	var pty bool
	var jsonl bool
	var termEnv string
	var raw bool
	var stderrToStdout bool

	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Execute a shell command via SessionService and stream structured events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}

			ss := xrunnerv1.NewSessionServiceClient(conn)
			sid := strings.TrimSpace(sessionID)
			if sid == "" {
				created, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{WorkspaceRoot: "/"})
				if err != nil {
					return err
				}
				sid = created.GetSession().GetSessionId()
				if !raw {
					fmt.Fprintf(os.Stderr, "session: %s\n", sid)
				}
			}

			env, err := parseKeyValue(envItems)
			if err != nil {
				return err
			}
			if pty {
				applyTerminalDefaults(env, termEnv)
			}
			argsJSON := buildShellArgsJSON(cmdStr, cwd, env, pty)

			chat, err := ss.Interact(ctx)
			if err != nil {
				return err
			}
			if err := chat.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{SessionId: sid}}}); err != nil {
				return err
			}
			if err := chat.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{Name: "shell", ArgsJson: argsJSON}}}); err != nil {
				return err
			}

			started := time.Now()
			marshal := protojson.MarshalOptions{UseProtoNames: true}
			stdoutW := io.Writer(os.Stdout)
			stderrW := io.Writer(os.Stderr)
			if stderrToStdout {
				stderrW = os.Stdout
			}
			bwOut := newBatchWriter(stdoutW, 8*time.Millisecond, 256*1024)
			bwErr := newBatchWriter(stderrW, 8*time.Millisecond, 256*1024)
			hopts := humanEventOpts{raw: raw, stderrToStdout: stderrToStdout, out: bwOut, err: bwErr}

			for {
				ev, err := chat.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if ev.GetToolCallResult() != nil {
					if jsonl {
						b, merr := marshal.Marshal(ev)
						if merr != nil {
							return merr
						}
						if _, werr := os.Stdout.Write(append(b, '\n')); werr != nil && errors.Is(werr, syscall.EPIPE) {
							return nil
						}
					} else {
						printEventHuman(ev, hopts)
					}
					if ev.GetToolCallResult().GetExitCode() != 0 {
						bwOut.Close()
						bwErr.Close()
						return fmt.Errorf("remote exit %d", ev.GetToolCallResult().GetExitCode())
					}
					if !jsonl && !raw {
						bwOut.Close()
						bwErr.Close()
						fmt.Fprintf(os.Stderr, "done exit=0 dur=%s out=%dB err=%dB\n", time.Since(started).Truncate(time.Millisecond), bwOut.TotalBytes(), bwErr.TotalBytes())
					} else {
						bwOut.Close()
						bwErr.Close()
					}
					return nil
				}
				if jsonl {
					b, err := marshal.Marshal(ev)
					if err != nil {
						return err
					}
					if _, werr := os.Stdout.Write(append(b, '\n')); werr != nil && errors.Is(werr, syscall.EPIPE) {
						return nil
					}
				} else {
					printEventHuman(ev, hopts)
				}
			}
		},
	}

	flags.bind(cmd)
	cmd.Flags().StringVar(&sessionID, "session", "", "existing session id (creates one if empty)")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "shell command to run on remote")
	cmd.Flags().StringVar(&cwd, "cwd", "", "remote working directory")
	cmd.Flags().StringArrayVar(&envItems, "env", nil, "remote environment variable KEY=VALUE (repeatable)")
	cmd.Flags().BoolVar(&pty, "pty", true, "run command under a PTY (better for interactive-ish output)")
	cmd.Flags().BoolVar(&jsonl, "jsonl", false, "emit AgentEvent JSON lines to stdout")
	cmd.Flags().StringVar(&termEnv, "term", "", "TERM value injected when --pty=true (defaults to local TERM or xterm-256color)")
	cmd.Flags().BoolVar(&raw, "raw", false, "print only tool output bytes (no status lines)")
	cmd.Flags().BoolVar(&stderrToStdout, "stderr-to-stdout", false, "merge remote stderr into stdout")
	_ = cmd.MarkFlagRequired("cmd")
	return cmd
}

func newSSHSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage remote SessionService sessions",
	}
	cmd.AddCommand(newSSHSessionListCmd())
	cmd.AddCommand(newSSHSessionCreateCmd())
	return cmd
}

func newSSHSessionListCmd() *cobra.Command {
	var flags sshRemoteFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}
			ss := xrunnerv1.NewSessionServiceClient(conn)
			resp, err := ss.ListSessions(ctx, &xrunnerv1.ListSessionsRequest{})
			if err != nil {
				return err
			}
			for _, s := range resp.GetSessions() {
				fmt.Fprintf(os.Stdout, "%s\t%s\t%s\n", s.GetSessionId(), s.GetCreatedAt().AsTime().Format(time.RFC3339), s.GetWorkspaceRoot())
			}
			return nil
		},
	}
	flags.bind(cmd)
	return cmd
}

func newSSHSessionCreateCmd() *cobra.Command {
	var flags sshRemoteFlags
	var workspaceRoot string
	var toolBundle string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			conn, cleanup, err := flags.connect(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(ctx, conn, flags.host); err != nil {
					return err
				}
			}
			ss := xrunnerv1.NewSessionServiceClient(conn)
			resp, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{
				WorkspaceRoot:  workspaceRoot,
				ToolBundlePath: toolBundle,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, resp.GetSession().GetSessionId())
			return nil
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&workspaceRoot, "workspace-root", "/", "workspace root for file ops/tools on remote")
	cmd.Flags().StringVar(&toolBundle, "tool-bundle", "", "tool bundle path on remote")
	return cmd
}

func parseKeyValue(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid KEY=VALUE: %q", item)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

func buildShellArgsJSON(cmdStr, cwd string, env map[string]string, pty bool) string {
	// Small hand-built JSON to avoid pulling extra deps in cmd/xrunner.
	var b strings.Builder
	b.WriteString(`{"cmd":`)
	b.WriteString(strconv.Quote(cmdStr))
	if strings.TrimSpace(cwd) != "" {
		b.WriteString(`,"cwd":`)
		b.WriteString(strconv.Quote(cwd))
	}
	if len(env) > 0 {
		first := true
		b.WriteString(`,"env":{`)
		for k, v := range env {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(strconv.Quote(k))
			b.WriteByte(':')
			b.WriteString(strconv.Quote(v))
		}
		b.WriteString("}")
	}
	b.WriteString(`,"pty":`)
	if pty {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString("}")
	return b.String()
}

func runShellOnce(ctx context.Context, ss xrunnerv1.SessionServiceClient, sessionID, cmdStr string, pty bool) (stdout string, stderr string, exitCode int32, runErr error) {
	chat, err := ss.Interact(ctx)
	if err != nil {
		return "", "", 1, err
	}
	if err := chat.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{SessionId: sessionID}}}); err != nil {
		return "", "", 1, err
	}
	argsJSON := buildShellArgsJSON(cmdStr, "", map[string]string{}, pty)
	if err := chat.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{Name: "shell", ArgsJson: argsJSON}}}); err != nil {
		return "", "", 1, err
	}

	var toolID string
	var outB bytes.Buffer
	var errB bytes.Buffer
	for {
		ev, err := chat.Recv()
		if err != nil {
			return outB.String(), errB.String(), 1, err
		}
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_ToolCallStarted:
			if k.ToolCallStarted.GetName() == "shell" && toolID == "" {
				toolID = k.ToolCallStarted.GetToolCallId()
			}
		case *xrunnerv1.AgentEvent_ToolOutputChunk:
			if toolID == "" {
				toolID = k.ToolOutputChunk.GetToolCallId()
			}
			if k.ToolOutputChunk.GetToolCallId() != toolID {
				continue
			}
			stream := strings.ToLower(strings.TrimSpace(k.ToolOutputChunk.GetStream()))
			if stream == "stderr" {
				_, _ = errB.Write(k.ToolOutputChunk.GetData())
			} else {
				_, _ = outB.Write(k.ToolOutputChunk.GetData())
			}
		case *xrunnerv1.AgentEvent_ToolCallResult:
			if toolID == "" {
				toolID = k.ToolCallResult.GetToolCallId()
			}
			if k.ToolCallResult.GetToolCallId() != toolID {
				continue
			}
			return outB.String(), errB.String(), k.ToolCallResult.GetExitCode(), nil
		}
	}
}

type humanEventOpts struct {
	raw            bool
	stderrToStdout bool
	out            io.Writer
	err            io.Writer
}

func printEventHuman(ev *xrunnerv1.AgentEvent, opts humanEventOpts) {
	switch k := ev.GetKind().(type) {
	case *xrunnerv1.AgentEvent_ToolOutputChunk:
		streamName := strings.ToLower(strings.TrimSpace(k.ToolOutputChunk.GetStream()))
		if streamName == "stderr" {
			_, _ = opts.err.Write(k.ToolOutputChunk.GetData())
			return
		}
		_, _ = opts.out.Write(k.ToolOutputChunk.GetData())
	case *xrunnerv1.AgentEvent_Error:
		if !opts.raw {
			fmt.Fprintf(os.Stderr, "error: %s\n", k.Error.GetMessage())
		}
	}
}

func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForTCP(ctx context.Context, addr string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for tcp %s", addr)
		case <-ticker.C:
			c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				return nil
			}
		}
	}
}

func makeStdinRaw() (func(), error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}, nil
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, oldState) }, nil
}

func termSize() (cols, rows int) {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return 120, 30
	}
	c, r, err := term.GetSize(fd)
	if err != nil || c <= 0 || r <= 0 {
		return 120, 30
	}
	return c, r
}

func applyTerminalDefaults(env map[string]string, overrideTERM string) {
	if env == nil {
		return
	}

	termVal := strings.TrimSpace(overrideTERM)
	if termVal == "" {
		termVal = strings.TrimSpace(os.Getenv("TERM"))
	}
	termLower := strings.ToLower(termVal)
	if termVal == "" || termLower == "unknown" || termLower == "dumb" {
		termVal = "xterm-256color"
	}

	hasTERM := false
	for k := range env {
		if strings.EqualFold(k, "TERM") {
			hasTERM = true
			break
		}
	}
	if !hasTERM {
		env["TERM"] = termVal
	}

	hasLocale := false
	for k := range env {
		if strings.EqualFold(k, "LC_ALL") || strings.EqualFold(k, "LANG") {
			hasLocale = true
			break
		}
	}
	if !hasLocale {
		env["LC_ALL"] = "C.UTF-8"
		if v := strings.TrimSpace(os.Getenv("LANG")); v != "" {
			env["LANG"] = v
		} else {
			env["LANG"] = "C.UTF-8"
		}
	}

	copyIfUnset(env, "COLORTERM")
	copyIfUnset(env, "TERM_PROGRAM")
	copyIfUnset(env, "TERM_PROGRAM_VERSION")
	copyIfUnset(env, "VTE_VERSION")
}

func shellEscapeArg(s string) string {
	// Single-quote escape for sh -lc arguments.
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func copyIfUnset(env map[string]string, key string) {
	for k := range env {
		if strings.EqualFold(k, key) {
			return
		}
	}
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		env[key] = v
	}
}

func runLocal(ctx context.Context, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("%s %v: %w\n%s", name, args, err, out)
		}
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

type batchWriter struct {
	dst        io.Writer
	flushEvery time.Duration
	maxBuf     int

	ch   chan []byte
	done chan struct{}

	total atomic.Uint64
	once  sync.Once
}

func newBatchWriter(dst io.Writer, flushEvery time.Duration, maxBuf int) *batchWriter {
	if flushEvery <= 0 {
		flushEvery = 8 * time.Millisecond
	}
	if maxBuf <= 0 {
		maxBuf = 256 * 1024
	}
	bw := &batchWriter{
		dst:        dst,
		flushEvery: flushEvery,
		maxBuf:     maxBuf,
		ch:         make(chan []byte, 128),
		done:       make(chan struct{}),
	}
	go bw.loop()
	return bw
}

func (b *batchWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	cp := append([]byte(nil), p...)
	b.ch <- cp
	b.total.Add(uint64(len(p)))
	return len(p), nil
}

func (b *batchWriter) TotalBytes() uint64 {
	return b.total.Load()
}

func (b *batchWriter) Close() {
	b.once.Do(func() {
		close(b.ch)
		<-b.done
	})
}

func (b *batchWriter) loop() {
	defer close(b.done)
	var buf bytes.Buffer
	ticker := time.NewTicker(b.flushEvery)
	defer ticker.Stop()

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		_, _ = b.dst.Write(buf.Bytes())
		buf.Reset()
	}

	for {
		select {
		case p, ok := <-b.ch:
			if !ok {
				flush()
				return
			}
			if len(p) > b.maxBuf {
				flush()
				_, _ = b.dst.Write(p)
				continue
			}
			if buf.Len()+len(p) > b.maxBuf {
				flush()
			}
			_, _ = buf.Write(p)
		case <-ticker.C:
			flush()
		}
	}
}
