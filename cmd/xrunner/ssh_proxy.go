package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newSSHProxyCmd() *cobra.Command {
	var flags sshRemoteFlags
	var localPort int
	var export bool
	var sshArgs []string

	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Start a persistent local proxy (SSH forward) to xrunner-remote",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			if strings.TrimSpace(flags.host) == "" {
				return fmt.Errorf("--host is required")
			}
			if err := ensureRemoteDaemonViaSSH(ctx, flags); err != nil {
				return err
			}

			port := localPort
			if port == 0 {
				p, err := freeLocalPort()
				if err != nil {
					return err
				}
				port = p
			}

			localAddr := fmt.Sprintf("127.0.0.1:%d", port)

			args := []string{
				"-o", "ExitOnForwardFailure=yes",
				"-o", "ServerAliveInterval=10",
				"-o", "ServerAliveCountMax=3",
			}
			if flags.batchMode {
				args = append(args, "-o", "BatchMode=yes")
			}
			args = append(args, sshArgs...)
			args = append(args,
				"-L", fmt.Sprintf("%s:127.0.0.1:%d", localAddr, flags.remotePort),
				"-N", flags.host,
			)
			sshCmd := exec.CommandContext(ctx, "ssh", args...)
			sshCmd.Stdout = os.Stderr
			sshCmd.Stderr = os.Stderr
			if err := sshCmd.Start(); err != nil {
				return err
			}

			// Wait for the forwarded port to accept and the daemon to respond.
			waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
			defer waitCancel()
			if err := waitForTCP(waitCtx, localAddr); err != nil {
				_ = sshCmd.Process.Kill()
				return err
			}
			conn, err := dialRemote(ctx, localAddr)
			if err == nil {
				_ = pingRemote(ctx, conn, flags.host)
				_ = conn.Close()
			}

			line := fmt.Sprintf("XRUNNER_SSH_PROXY=%s", localAddr)
			if export {
				line = "export " + line
			}
			fmt.Fprintln(os.Stdout, line)

			// Block until ssh exits or we are interrupted.
			err = sshCmd.Wait()
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return err
		},
	}

	flags.bind(cmd)
	cmd.Flags().IntVar(&localPort, "local-port", 0, "local port for the proxy (0 chooses a free port)")
	cmd.Flags().BoolVar(&export, "export", true, "print an `export XRUNNER_SSH_PROXY=...` line")
	cmd.Flags().StringArrayVar(&sshArgs, "ssh-arg", nil, "extra args passed to ssh (repeatable, e.g. --ssh-arg=-v or --ssh-arg=-i --ssh-arg=~/.ssh/id_ed25519)")
	return cmd
}
