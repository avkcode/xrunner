package main

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

type sshUninstallFlags struct {
	sshArgs    []string
	dryRun     bool
	mode       string // auto|systemd|user
	removeUser bool
}

func newSSHUninstallCmd() *cobra.Command {
	var f sshUninstallFlags

	cmd := &cobra.Command{
		Use:   "uninstall <user@host>",
		Short: "Uninstall xrunner from a host (purge services, config, state)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := strings.TrimSpace(args[0])
			if host == "" {
				return fmt.Errorf("host is required")
			}

			if f.dryRun {
				fmt.Fprintln(os.Stdout, "uninstall plan (dry-run):")
				fmt.Fprintf(os.Stdout, "- stop services / kill processes\n")
				fmt.Fprintf(os.Stdout, "- remove systemd units + env files\n")
				fmt.Fprintf(os.Stdout, "- remove binaries referenced by unit ExecStart\n")
				fmt.Fprintf(os.Stdout, "- remove state dirs (/var/lib/xrunner, ~/.xrunner)\n")
				if f.removeUser {
					fmt.Fprintf(os.Stdout, "- remove xrunner user/group\n")
				}
				fmt.Fprintln(os.Stdout, "pass --dry-run=false to execute")
				return nil
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			script, err := renderRemoteUninstallScript(f.mode, f.removeUser)
			if err != nil {
				return err
			}
			allowSudo := strings.ToLower(strings.TrimSpace(f.mode)) != "user"
			if err := runRemoteScript(ctx, host, script, f.sshArgs, allowSudo); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "[xrunner] uninstall complete")
			return nil
		},
	}

	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what would happen without executing")
	cmd.Flags().StringArrayVar(&f.sshArgs, "ssh-arg", nil, "extra args passed to ssh (repeatable, e.g. --ssh-arg=-i --ssh-arg=~/.ssh/id_ed25519)")
	cmd.Flags().StringVar(&f.mode, "install-mode", "auto", "uninstall mode: auto|systemd|user")
	cmd.Flags().BoolVar(&f.removeUser, "remove-user", true, "remove xrunner user/group (systemd mode only; best-effort)")
	return cmd
}

func renderRemoteUninstallScript(mode string, removeUser bool) ([]byte, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = "auto"
	}
	switch m {
	case "auto", "systemd", "user":
	default:
		return nil, fmt.Errorf("unsupported --install-mode: %q (expected auto|systemd|user)", mode)
	}

	var b bytes.Buffer
	_, _ = b.WriteString("set -euo pipefail\n")
	_, _ = b.WriteString("umask 022\n")
	_, _ = b.WriteString("MODE=" + shQuote(m) + "\n")
	if removeUser {
		_, _ = b.WriteString("REMOVE_USER=1\n")
	} else {
		_, _ = b.WriteString("REMOVE_USER=0\n")
	}

	// Helper: stop/disable systemd unit if present.
	_, _ = b.WriteString("stop_disable() {\n")
	_, _ = b.WriteString("  u=\"$1\"\n")
	_, _ = b.WriteString("  systemctl stop \"$u\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("  systemctl disable \"$u\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("  systemctl reset-failed \"$u\" >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("}\n")

	// Helper: attempt to extract ExecStart binary path from a unit.
	_, _ = b.WriteString("unit_bin() {\n")
	_, _ = b.WriteString("  u=\"$1\"\n")
	_, _ = b.WriteString("  systemctl cat \"$u\" 2>/dev/null | sed -n 's/^ExecStart=\\([^[:space:]]\\+\\).*/\\1/p' | head -n1 || true\n")
	_, _ = b.WriteString("}\n")

	// Mode detection: if systemd units exist, treat as systemd install.
	_, _ = b.WriteString("detect_mode() {\n")
	_, _ = b.WriteString("  if [ \"$MODE\" != \"auto\" ]; then echo \"$MODE\"; return 0; fi\n")
	_, _ = b.WriteString("  if command -v systemctl >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("    if systemctl status xrunner-api.service >/dev/null 2>&1 || systemctl status xrunner-worker.service >/dev/null 2>&1 || systemctl status xrunner-remote.service >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("      echo systemd; return 0\n")
	_, _ = b.WriteString("    fi\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  echo user\n")
	_, _ = b.WriteString("}\n")
	_, _ = b.WriteString("MODE=$(detect_mode)\n")

	_, _ = b.WriteString("echo \"[xrunner] uninstall mode=$MODE\" >&2\n")

	// Always try to stop stray processes by name (best-effort).
	_, _ = b.WriteString("pkill -f xrunner-remote >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("pkill -f xrunner-api >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("pkill -f xrunner-worker >/dev/null 2>&1 || true\n")

	// User-mode purge: remove ~/.xrunner (for the invoking user).
	_, _ = b.WriteString("purge_user() {\n")
	_, _ = b.WriteString("  if [ -d \"$HOME/.xrunner\" ]; then rm -rf \"$HOME/.xrunner\"; fi\n")
	_, _ = b.WriteString("}\n")

	// Systemd purge: remove units/env/config/state/bins.
	_, _ = b.WriteString("purge_systemd() {\n")
	_, _ = b.WriteString("  if command -v systemctl >/dev/null 2>&1; then\n")
	_, _ = b.WriteString("    stop_disable xrunner-remote.service\n")
	_, _ = b.WriteString("    stop_disable xrunner-api.service\n")
	_, _ = b.WriteString("    stop_disable xrunner-worker.service\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("  bin_api=$(unit_bin xrunner-api.service)\n")
	_, _ = b.WriteString("  bin_worker=$(unit_bin xrunner-worker.service)\n")
	_, _ = b.WriteString("  bin_remote=$(unit_bin xrunner-remote.service)\n")
	_, _ = b.WriteString("  rm -f /etc/systemd/system/xrunner-api.service /etc/systemd/system/xrunner-worker.service /etc/systemd/system/xrunner-remote.service || true\n")
	_, _ = b.WriteString("  rm -f /lib/systemd/system/xrunner-api.service /lib/systemd/system/xrunner-worker.service /lib/systemd/system/xrunner-remote.service || true\n")
	_, _ = b.WriteString("  rm -f /etc/default/xrunner-api /etc/default/xrunner-worker /etc/default/xrunner-remote || true\n")
	_, _ = b.WriteString("  rm -rf /etc/xrunner || true\n")
	_, _ = b.WriteString("  rm -rf /var/lib/xrunner || true\n")
	_, _ = b.WriteString("  rm -rf /root/.xrunner || true\n")
	_, _ = b.WriteString("  if [ -n \"$bin_api\" ] && [ -f \"$bin_api\" ]; then rm -f \"$bin_api\" || true; fi\n")
	_, _ = b.WriteString("  if [ -n \"$bin_worker\" ] && [ -f \"$bin_worker\" ]; then rm -f \"$bin_worker\" || true; fi\n")
	_, _ = b.WriteString("  if [ -n \"$bin_remote\" ] && [ -f \"$bin_remote\" ]; then rm -f \"$bin_remote\" || true; fi\n")
	_, _ = b.WriteString("  # Also remove standard bootstrap paths if present.\n")
	_, _ = b.WriteString("  rm -f /usr/local/bin/xrunner-api /usr/local/bin/xrunner-worker /usr/local/bin/xrunner-remote || true\n")
	_, _ = b.WriteString("  if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload || true; fi\n")
	_, _ = b.WriteString("  if [ \"$REMOVE_USER\" = \"1\" ]; then\n")
	_, _ = b.WriteString("    userdel -r xrunner >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("    groupdel xrunner >/dev/null 2>&1 || true\n")
	_, _ = b.WriteString("  fi\n")
	_, _ = b.WriteString("}\n")

	_, _ = b.WriteString("case \"$MODE\" in\n")
	_, _ = b.WriteString("  systemd) purge_systemd ;;\n")
	_, _ = b.WriteString("  user) purge_user ;;\n")
	_, _ = b.WriteString("  *) echo \"[xrunner] unexpected mode=$MODE\" >&2; exit 2 ;;\n")
	_, _ = b.WriteString("esac\n")

	// Also purge user dir for the executing user in systemd mode (common leftover).
	_, _ = b.WriteString("purge_user || true\n")
	_, _ = b.WriteString("echo \"[xrunner] uninstall done\" >&2\n")
	return b.Bytes(), nil
}
