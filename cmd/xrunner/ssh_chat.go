package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type remoteFileWriter interface {
	WriteFileAtomic(context.Context, *xrunnerv1.WriteFileRequest, ...grpc.CallOption) (*emptypb.Empty, error)
}

func expandHome(path string) string {
	if path == "~" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return h
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func uploadToolBundleIfLocal(ctx context.Context, filesc remoteFileWriter, toolBundlePath string) (remotePath string, uploaded bool, _ error) {
	tb := strings.TrimSpace(toolBundlePath)
	if tb == "" {
		return "", false, nil
	}

	localPath := expandHome(tb)
	info, err := os.Stat(localPath)
	if err != nil {
		// Not present locally => treat as a remote path.
		if errors.Is(err, os.ErrNotExist) {
			return tb, false, nil
		}
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("tool bundle: %q is a directory", localPath)
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", false, err
	}

	sum := sha256.Sum256(data)
	short := hex.EncodeToString(sum[:])[:12]
	base := filepath.Base(localPath)
	remote := filepath.ToSlash(filepath.Join("/tmp/xrunner/tool-bundles", base+"."+short+".json"))

	if _, err := filesc.WriteFileAtomic(ctx, &xrunnerv1.WriteFileRequest{
		Path: remote,
		Data: data,
		Mode: 0o644,
	}); err != nil {
		return "", false, err
	}
	return remote, true, nil
}

func newSSHChatCmd() *cobra.Command {
	var flags sshRemoteFlags
	var sessionID string
	var afterEventID int64
	var pty bool
	var raw bool
	var jsonl bool
	var promptOnce string
	var promptTimeout time.Duration
	var promptFile string
	var promptStdin bool
	var promptJSON bool
	var promptQuiet bool
	var promptSession string
	var resume string
	var tmuxTimeout time.Duration
	var shellQuiet bool
	var toolBundle string

	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interactive chat-like loop over SessionService (Codex-style CLI UX)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			baseCtx := cmd.Context()

			conn, cleanup, err := flags.connect(baseCtx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if flags.resolvedProxyAddr() == "" {
				if err := pingRemote(baseCtx, conn, flags.host); err != nil {
					return err
				}
			}

			ss := xrunnerv1.NewSessionServiceClient(conn)
			shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
			filesc := xrunnerv1.NewRemoteFileServiceClient(conn)
			if strings.TrimSpace(promptSession) != "" && strings.TrimSpace(sessionID) != "" && strings.TrimSpace(promptSession) != strings.TrimSpace(sessionID) {
				return fmt.Errorf("--prompt-session and --session must match when both are set")
			}
			if strings.TrimSpace(promptSession) != "" {
				sessionID = strings.TrimSpace(promptSession)
			}
			sid := strings.TrimSpace(sessionID)
			if sid == "" {
				tb := strings.TrimSpace(toolBundle)
				if tb == "" {
					tb = strings.TrimSpace(os.Getenv("AGENT_TOOL_SPEC"))
				}
				if tb != "" {
					uploadedPath, uploaded, err := uploadToolBundleIfLocal(baseCtx, filesc, tb)
					if err != nil {
						return err
					}
					if uploaded {
						if !promptQuiet {
							fmt.Fprintf(os.Stderr, "tool bundle: uploaded %s -> %s\n", tb, uploadedPath)
						}
					}
					tb = uploadedPath
				}
				created, err := ss.CreateSession(baseCtx, &xrunnerv1.CreateSessionRequest{
					WorkspaceRoot:  "/",
					ToolBundlePath: tb,
				})
				if err != nil {
					return err
				}
				sid = created.GetSession().GetSessionId()
				if !promptQuiet {
					fmt.Fprintf(os.Stderr, "session: %s\n", sid)
				}
			}

			if promptJSON && jsonl {
				return fmt.Errorf("--prompt-json is not compatible with --jsonl")
			}
			if promptStdin && strings.TrimSpace(promptFile) != "" {
				return fmt.Errorf("--prompt-stdin and --prompt-file are mutually exclusive")
			}
			if (promptStdin || strings.TrimSpace(promptFile) != "") && strings.TrimSpace(promptOnce) != "" {
				return fmt.Errorf("--prompt cannot be combined with --prompt-stdin/--prompt-file")
			}

			askText := strings.TrimSpace(promptOnce)
			if askText == "" && strings.TrimSpace(promptFile) != "" {
				b, err := os.ReadFile(strings.TrimSpace(promptFile))
				if err != nil {
					return fmt.Errorf("prompt-file: %w", err)
				}
				askText = strings.TrimSpace(string(b))
			}
			if askText == "" && promptStdin {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("prompt-stdin: %w", err)
				}
				askText = strings.TrimSpace(string(b))
			}
			if askText != "" {
				// In one-shot prompt mode, Ctrl-C / Ctrl-Z should exit immediately.
				// Use a signal-canceled context and avoid the interactive cancel UX.
				ctx, stop := signal.NotifyContext(baseCtx, os.Interrupt, syscall.SIGTERM, syscall.SIGTSTP)
				defer stop()
				if promptJSON {
					promptQuiet = true
				}
				timeout := promptTimeout
				if timeout <= 0 {
					timeout = 60 * time.Second
				}
				askCtx, askCancel := context.WithTimeout(ctx, timeout)
				defer askCancel()
					err := runPromptOnce(askCtx, ss, sid, afterEventID, askText, jsonl, promptJSON, promptQuiet)
					if err == nil {
						return nil
					}
					if errors.Is(err, context.DeadlineExceeded) {
						return fmt.Errorf("prompt timed out after %s (use --prompt-timeout to increase): %w", timeout, err)
					}
					if st, ok := status.FromError(err); ok && st.Code() == codes.DeadlineExceeded {
						return fmt.Errorf("prompt timed out after %s (use --prompt-timeout to increase): %w", timeout, err)
					}
					if errors.Is(err, context.Canceled) {
						return err
					}
					return err
				}

			ctx, cancel := context.WithCancel(baseCtx)
			defer cancel()
			sigCh := make(chan os.Signal, 4)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGTSTP)
			defer signal.Stop(sigCh)

			printMu := &sync.Mutex{}
			sendMu := &sync.Mutex{}

			resumeName := strings.TrimSpace(resume)
			var shell *chatShell
			if resumeName != "" {
				cols, rows := termSize()
				sh, err := newChatShell(ctx, shellc, filesc, resumeName, uint32(cols), uint32(rows), printMu)
				if err != nil {
					return err
				}
				shell = sh
				shell.shellQuiet = shellQuiet
				fmt.Fprintf(os.Stderr, "shell: %s\n", resumeName)
			}

			stream, err := ss.Interact(ctx)
			if err != nil {
				return err
			}
			send := func(msg *xrunnerv1.ClientMsg) error {
				sendMu.Lock()
				defer sendMu.Unlock()
				return stream.Send(msg)
			}
			if err := send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{
				SessionId:    sid,
				AfterEventId: afterEventID,
			}}}); err != nil {
				return err
			}

			prompt := func() {
				if raw || jsonl {
					return
				}
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return
				}
				printMu.Lock()
				defer printMu.Unlock()
				_, _ = fmt.Fprint(os.Stdout, "xrunner> ")
			}

			marshal := protojson.MarshalOptions{UseProtoNames: true}
			printer := newStreamingChatPrinter(os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdout.Fd())), raw, printMu)

			// Signals: Ctrl-C cancels current generation (double Ctrl-C exits); Ctrl-Z exits instead of suspending.
			go func() {
				var lastInterrupt time.Time
				for {
					select {
					case <-ctx.Done():
						return
					case sig := <-sigCh:
						switch sig {
						case os.Interrupt:
							now := time.Now()
							if !lastInterrupt.IsZero() && now.Sub(lastInterrupt) < 900*time.Millisecond {
								cancel()
								return
							}
							lastInterrupt = now
							_ = send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Cancel{Cancel: &xrunnerv1.Cancel{}}})
							if term.IsTerminal(int(os.Stdout.Fd())) && !raw && !jsonl {
								printMu.Lock()
								_, _ = fmt.Fprint(os.Stdout, "\n")
								printMu.Unlock()
							}
						case syscall.SIGTSTP, syscall.SIGTERM:
							cancel()
							return
						default:
						}
					}
				}
			}()
			recvErrCh := make(chan error, 1)
			go func() {
				defer close(recvErrCh)
				for {
					ev, err := stream.Recv()
					if err != nil {
						recvErrCh <- err
						return
					}
					printMu.Lock()
					if jsonl {
						b, merr := marshal.Marshal(ev)
						if merr == nil {
							_, _ = os.Stdout.Write(append(b, '\n'))
						}
						printMu.Unlock()
						continue
					}
					suppressPrompt := printer.Handle(ev)
					printMu.Unlock()
					if !suppressPrompt {
						prompt()
					}
				}
			}()

			prompt()
			linesCh, stdinErrCh := startStdinReader(ctx)
			for {
				select {
				case <-ctx.Done():
					printer.Close()
					_ = stream.CloseSend()
					if term.IsTerminal(int(os.Stdout.Fd())) && !raw && !jsonl {
						printMu.Lock()
						_, _ = fmt.Fprint(os.Stdout, "\n")
						printMu.Unlock()
					}
					return nil
				case err := <-recvErrCh:
					if errors.Is(err, io.EOF) || err == nil {
						return nil
					}
					return err
				case err := <-stdinErrCh:
					if err != nil && !errors.Is(err, context.Canceled) {
						return err
					}
					_ = stream.CloseSend()
					return nil
				case line, ok := <-linesCh:
					if !ok {
						printer.Close()
						_ = stream.CloseSend()
						// Give the receive loop a brief window to flush trailing output.
						select {
						case err := <-recvErrCh:
							if err == nil || errors.Is(err, io.EOF) {
								return nil
							}
							return err
						case <-time.After(750 * time.Millisecond):
							return nil
						}
					}
					line = strings.TrimSpace(line)
					if line == "" {
						prompt()
						continue
					}
					if strings.HasPrefix(line, "/") {
						handled, promptNow, err := handleChatCommand(ctx, ss, send, line, pty, raw, shellc, sid, shell, tmuxTimeout, printMu)
						if err != nil {
							return err
						} else if handled {
							if line == "/exit" || line == "/quit" {
								printer.Close()
								_ = stream.CloseSend()
								select {
								case err := <-recvErrCh:
									if err == nil || errors.Is(err, io.EOF) {
										return nil
									}
									return err
								case <-time.After(750 * time.Millisecond):
									return nil
								}
							}
							if promptNow {
								prompt()
							}
							continue
						}
					}
					if strings.HasPrefix(line, "!") {
						cmdStr := strings.TrimSpace(line[1:])
						if resumeName != "" {
							if shell == nil {
								return fmt.Errorf("shell not initialized")
							}
							rec, err := shell.Run(ctx, cmdStr, tmuxTimeout)
							if err != nil {
								return err
							}
							if !shell.shellQuiet {
								if err := shell.Replay(ctx, rec.GetCommandId(), os.Stdout); err != nil {
									return err
								}
							}
							prompt()
						} else {
							if err := sendShell(send, cmdStr, pty); err != nil {
								return err
							}
						}
						continue
					}
					if err := send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: line}}}); err != nil {
						return err
					}
					continue
				}
			}
		},
	}
	cmd.SilenceUsage = true

	flags.bind(cmd)
	cmd.Flags().StringVar(&sessionID, "session", "", "existing session id (creates one if empty)")
	cmd.Flags().Int64Var(&afterEventID, "after", 0, "replay events after this event id")
	cmd.Flags().BoolVar(&pty, "pty", true, "run /shell and !cmd using a PTY (fast, but stdout/stderr merged)")
	cmd.Flags().BoolVar(&raw, "raw", false, "print only tool output bytes (minimizes UI noise)")
	cmd.Flags().BoolVar(&jsonl, "jsonl", false, "emit AgentEvent JSON lines (disables prompt UX)")
	cmd.Flags().StringVar(&promptOnce, "prompt", "", "send a single prompt and exit (non-interactive)")
	cmd.Flags().DurationVar(&promptTimeout, "prompt-timeout", 3*time.Minute, "timeout for --prompt")
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "read the prompt from a local file (non-interactive)")
	cmd.Flags().BoolVar(&promptStdin, "prompt-stdin", false, "read the prompt from stdin (non-interactive)")
	cmd.Flags().BoolVar(&promptQuiet, "prompt-quiet", false, "suppress streaming deltas; print only the final assistant message")
	cmd.Flags().BoolVar(&promptJSON, "prompt-json", false, "print the final assistant message as JSON (implies --prompt-quiet)")
	cmd.Flags().StringVar(&promptSession, "prompt-session", "", "reuse an existing session id for --prompt/--prompt-file/--prompt-stdin")
	// Backwards-compatible alias.
	cmd.Flags().StringVar(&promptOnce, "ask", "", "alias for --prompt")
	cmd.Flags().DurationVar(&promptTimeout, "ask-timeout", 3*time.Minute, "alias for --prompt-timeout")
	_ = cmd.Flags().MarkDeprecated("ask", "use --prompt")
	_ = cmd.Flags().MarkDeprecated("ask-timeout", "use --prompt-timeout")
	cmd.Flags().StringVar(&resume, "resume", "", "send !cmd and /shell into a durable shell session name (tmux-backed)")
	cmd.Flags().DurationVar(&tmuxTimeout, "tmux-timeout", 10*time.Second, "timeout for a /shell command when using --resume")
	cmd.Flags().BoolVar(&shellQuiet, "shell-quiet", true, "when --resume is set, don't print command output; use /replay <id> (default true)")
	cmd.Flags().StringVar(&toolBundle, "tool-bundle", "", "tool bundle JSON path (default $AGENT_TOOL_SPEC); local paths are uploaded to the remote")
	return cmd
}

func runPromptOnce(ctx context.Context, ss xrunnerv1.SessionServiceClient, sessionID string, afterEventID int64, prompt string, jsonl bool, asJSON bool, quiet bool) error {
	stream, err := ss.Interact(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.CloseSend() }()

	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{
		SessionId:    sessionID,
		AfterEventId: afterEventID,
	}}}); err != nil {
		return err
	}
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: prompt}}}); err != nil {
		return err
	}

	marshal := protojson.MarshalOptions{UseProtoNames: true}
	eventsCh := make(chan *xrunnerv1.AgentEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(eventsCh)
		for {
			ev, rerr := stream.Recv()
			if rerr != nil {
				errCh <- rerr
				return
			}
			eventsCh <- ev
		}
	}()

	sawAssistant := false
	sawAssistantStream := false
	var assistantText strings.Builder

	flushJSON := func() {
		out, _ := json.Marshal(map[string]any{
			"session_id": sessionID,
			"text":       strings.TrimSpace(assistantText.String()),
		})
		_, _ = os.Stdout.Write(append(out, '\n'))
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rerr := <-errCh:
			if errors.Is(rerr, io.EOF) {
				if asJSON {
					flushJSON()
				}
				return nil
			}
			return rerr
		case ev, ok := <-eventsCh:
			if !ok {
				if asJSON {
					flushJSON()
				}
				return nil
			}
			if jsonl {
				b, merr := marshal.Marshal(ev)
				if merr == nil {
					_, _ = os.Stdout.Write(append(b, '\n'))
				}
			}

			switch k := ev.GetKind().(type) {
			case *xrunnerv1.AgentEvent_ToolOutputChunk:
				if k.ToolOutputChunk != nil && k.ToolOutputChunk.GetStream() == "assistant" {
					sawAssistantStream = true
					assistantText.Write(k.ToolOutputChunk.GetData())
					sawAssistant = true
					if !asJSON && !quiet {
						_, _ = os.Stdout.Write(k.ToolOutputChunk.GetData())
					}
				}
			case *xrunnerv1.AgentEvent_AssistantMessage:
				if !sawAssistantStream && k.AssistantMessage != nil {
					sawAssistant = true
					assistantText.Reset()
					assistantText.WriteString(k.AssistantMessage.GetText())
					if !asJSON {
						_, _ = fmt.Fprintln(os.Stdout, k.AssistantMessage.GetText())
					}
				}
			case *xrunnerv1.AgentEvent_Error:
				if k.Error != nil && strings.TrimSpace(k.Error.GetMessage()) != "" {
					return errors.New(strings.TrimSpace(k.Error.GetMessage()))
				}
			case *xrunnerv1.AgentEvent_Status:
				phase := ""
				if k.Status != nil {
					phase = strings.TrimSpace(k.Status.GetPhase())
				}
				if phase == "idle" && sawAssistant {
					if asJSON {
						flushJSON()
						return nil
					}
					if sawAssistantStream {
						if quiet {
							_, _ = fmt.Fprintln(os.Stdout, strings.TrimSpace(assistantText.String()))
						} else {
							_, _ = fmt.Fprintln(os.Stdout)
						}
					}
					return nil
				}
			default:
			}
		}
	}
}

type sessionLister interface {
	ListSessions(context.Context, *xrunnerv1.ListSessionsRequest, ...grpc.CallOption) (*xrunnerv1.ListSessionsResponse, error)
}

type streamSender func(*xrunnerv1.ClientMsg) error

func handleChatCommand(ctx context.Context, ss sessionLister, send streamSender, line string, pty bool, raw bool, shellc xrunnerv1.RemoteShellServiceClient, _ string, shell *chatShell, tmuxTimeout time.Duration, printMu *sync.Mutex) (handled bool, promptNow bool, err error) {
	switch {
	case line == "/exit" || line == "/quit":
		return true, false, nil
	case line == "/help":
		if !raw {
			fmt.Fprintln(os.Stdout, "commands:")
			fmt.Fprintln(os.Stdout, "  /help                show this help")
			fmt.Fprintln(os.Stdout, "  /shell <cmd>          run remote shell tool (or durable shell when --resume is set)")
			fmt.Fprintln(os.Stdout, "  !<cmd>                shorthand for /shell")
			fmt.Fprintln(os.Stdout, "  /write <path> <text>  write a file (write_file_atomic tool)")
			fmt.Fprintln(os.Stdout, "  /sessions             list server sessions")
			fmt.Fprintln(os.Stdout, "  /timeline [n]         list last N durable commands (requires --resume)")
			fmt.Fprintln(os.Stdout, "  /replay <id>          print output for a command id (requires --resume)")
			fmt.Fprintln(os.Stdout, "  /rerun <id>           rerun a command id (requires --resume)")
			fmt.Fprintln(os.Stdout, "  /artifacts <id>       show artifacts for a command id (requires --resume)")
			fmt.Fprintln(os.Stdout, "  /open <path:line>     fetch+open remote file locally (requires --resume)")
			fmt.Fprintln(os.Stdout, "  /exit | /quit         exit")
			fmt.Fprintln(os.Stdout, "flags:")
			fmt.Fprintln(os.Stdout, "  --resume <name>       send commands into a durable shell session")
			fmt.Fprintln(os.Stdout, "  --shell-quiet         don't auto-print durable command output")
		}
		return true, true, nil
	case strings.HasPrefix(line, "/write "):
		spec := strings.TrimSpace(strings.TrimPrefix(line, "/write "))
		if spec == "" {
			return true, true, fmt.Errorf("write: usage /write <path> <content>")
		}
		parts := strings.Fields(spec)
		if len(parts) < 2 {
			return true, true, fmt.Errorf("write: usage /write <path> <content>")
		}
		path := parts[0]
		i := strings.Index(spec, path)
		if i < 0 {
			return true, true, fmt.Errorf("write: invalid input")
		}
		content := strings.TrimSpace(spec[i+len(path):])
		if content == "" {
			return true, true, fmt.Errorf("write: content is required")
		}
		args, _ := json.Marshal(map[string]any{
			"path":    path,
			"content": content,
			"mode":    0,
		})
		return true, false, send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
			Name:     "write_file_atomic",
			ArgsJson: string(args),
		}}})
	case line == "/sessions":
		if ss == nil {
			return true, true, fmt.Errorf("sessions unavailable")
		}
		resp, err := ss.ListSessions(ctx, &xrunnerv1.ListSessionsRequest{})
		if err != nil {
			return true, true, err
		}
		printMu.Lock()
		defer printMu.Unlock()
		for _, s := range resp.GetSessions() {
			if s == nil {
				continue
			}
			tb := strings.TrimSpace(s.GetToolBundlePath())
			if tb != "" {
				tb = " tool_bundle=" + tb
			}
			fmt.Fprintf(os.Stdout, "session %s workspace=%s%s\n", s.GetSessionId(), s.GetWorkspaceRoot(), tb)
		}
		return true, true, nil
	case strings.HasPrefix(line, "/shell "):
		cmdStr := strings.TrimSpace(strings.TrimPrefix(line, "/shell "))
		if shell != nil {
			rec, err := shell.Run(ctx, cmdStr, tmuxTimeout)
			if err != nil {
				return true, true, err
			}
			if shell.shellQuiet {
				return true, true, nil
			}
			return true, true, shell.Replay(ctx, rec.GetCommandId(), os.Stdout)
		}
		return true, false, sendShell(send, cmdStr, pty)
	case line == "/shell":
		if shell != nil {
			return true, true, fmt.Errorf("shell: command is required")
		}
		return true, false, sendShell(send, "", pty)
	case strings.HasPrefix(line, "/timeline"):
		if shell == nil {
			return true, true, fmt.Errorf("timeline requires --resume")
		}
		n := 10
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			if v, err := strconv.Atoi(parts[1]); err == nil && v > 0 {
				n = v
			}
		}
		printMu.Lock()
		defer printMu.Unlock()
		for _, r := range shell.Timeline(ctx, n) {
			ts := ""
			if r.GetStartedAt() != nil {
				ts = r.GetStartedAt().AsTime().Format(time.RFC3339)
			}
			arts := ""
			if len(r.GetArtifactIds()) > 0 {
				arts = fmt.Sprintf(" artifacts=%d", len(r.GetArtifactIds()))
			}
			fmt.Fprintf(os.Stdout, "%s exit=%d bytes=%d%s id=%s cmd=%s\n", ts, r.GetExitCode(), r.GetOutputBytes(), arts, r.GetCommandId(), r.GetCommand())
		}
		return true, true, nil
	case strings.HasPrefix(line, "/replay "):
		if shell == nil {
			return true, true, fmt.Errorf("replay requires --resume")
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "/replay "))
		if id == "" {
			return true, true, fmt.Errorf("replay: command id is required")
		}
		return true, true, shell.Replay(ctx, id, os.Stdout)
	case strings.HasPrefix(line, "/rerun "):
		if shell == nil {
			return true, true, fmt.Errorf("rerun requires --resume")
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "/rerun "))
		if id == "" {
			return true, true, fmt.Errorf("rerun: command id is required")
		}
		rec, err := shell.Rerun(ctx, id, tmuxTimeout)
		if err != nil {
			return true, true, err
		}
		if shell.shellQuiet {
			return true, true, nil
		}
		return true, true, shell.Replay(ctx, rec.GetCommandId(), os.Stdout)
	case strings.HasPrefix(line, "/artifacts "):
		if shell == nil {
			return true, true, fmt.Errorf("artifacts requires --resume")
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "/artifacts "))
		if id == "" {
			return true, true, fmt.Errorf("artifacts: command id is required")
		}
		arts, err := shell.Artifacts(ctx, id, 50)
		if err != nil {
			return true, true, err
		}
		printMu.Lock()
		defer printMu.Unlock()
		for _, a := range arts {
			if a == nil {
				continue
			}
			fmt.Fprintf(os.Stdout, "artifact id=%s type=%s title=%s\n", a.GetId(), a.GetType(), a.GetTitle())
			if len(a.GetDataJson()) > 0 {
				fmt.Fprintf(os.Stdout, "%s\n", string(a.GetDataJson()))
			}
		}
		return true, true, nil
	case strings.HasPrefix(line, "/open "):
		if shell == nil {
			return true, true, fmt.Errorf("open requires --resume")
		}
		spec := strings.TrimSpace(strings.TrimPrefix(line, "/open "))
		if spec == "" {
			return true, true, fmt.Errorf("open: path:line is required")
		}
		return true, true, shell.OpenLocal(ctx, spec)
	case strings.HasPrefix(line, "/rerun-failing "):
		if shell == nil {
			return true, true, fmt.Errorf("rerun-failing requires --resume")
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "/rerun-failing "))
		if id == "" {
			return true, true, fmt.Errorf("rerun-failing: command id is required")
		}
		before, err := shell.getGoTestFailure(ctx, id)
		if err != nil {
			return true, true, err
		}
		if before == nil {
			return true, true, fmt.Errorf("rerun-failing: no go_test_failure artifact for %s", id)
		}
		newID, summaryCmd := shell.buildRerunFailingCommand(before)
		if summaryCmd == "" {
			return true, true, fmt.Errorf("rerun-failing: could not build rerun command")
		}
		rec, err := shell.Run(ctx, summaryCmd, tmuxTimeout)
		if err != nil {
			return true, true, err
		}
		after, _ := shell.getGoTestFailure(ctx, rec.GetCommandId())
		printMu.Lock()
		defer printMu.Unlock()
		fmt.Fprintf(os.Stdout, "rerun-failing: %s\n", summaryCmd)
		fmt.Fprintf(os.Stdout, "before: id=%s exit=%d failures=%s\n", id, shell.exitOf(id), describeGoTestFailure(before))
		fmt.Fprintf(os.Stdout, "after:  id=%s exit=%d failures=%s\n", rec.GetCommandId(), rec.GetExitCode(), describeGoTestFailure(after))
		_ = newID
		return true, true, nil
	case strings.HasPrefix(line, "/fix "):
		if shell == nil {
			return true, true, fmt.Errorf("fix requires --resume")
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "/fix "))
		if id == "" {
			return true, true, fmt.Errorf("fix: command id is required")
		}
		fail, err := shell.getGoTestFailure(ctx, id)
		if err != nil {
			return true, true, err
		}
		if fail == nil || len(fail.Files) == 0 {
			return true, true, fmt.Errorf("fix: no go_test_failure artifact with file locations for %s", id)
		}
		applied, msg, err := shell.tryHeuristicFix(ctx, fail.Files[0].File, fail.Files[0].Line, fail.Files[0].Text)
		if err != nil {
			return true, true, err
		}
		printMu.Lock()
		fmt.Fprintln(os.Stdout, msg)
		printMu.Unlock()
		if !applied {
			return true, true, nil
		}
		// After applying, rerun only failing test.
		rec, cmdStr := shell.rerunFailing(ctx, fail, tmuxTimeout)
		if rec == nil {
			return true, true, fmt.Errorf("fix: failed to rerun")
		}
		after, _ := shell.getGoTestFailure(ctx, rec.GetCommandId())
		printMu.Lock()
		defer printMu.Unlock()
		fmt.Fprintf(os.Stdout, "rerun: %s\n", cmdStr)
		fmt.Fprintf(os.Stdout, "before: id=%s exit=%d failures=%s\n", id, shell.exitOf(id), describeGoTestFailure(fail))
		fmt.Fprintf(os.Stdout, "after:  id=%s exit=%d failures=%s\n", rec.GetCommandId(), rec.GetExitCode(), describeGoTestFailure(after))
		return true, true, nil
	default:
		return false, false, nil
	}
}

func startStdinReader(ctx context.Context) (<-chan string, <-chan error) {
	lines := make(chan string, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(lines)
		defer close(errs)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			txt := sc.Text()
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case lines <- txt:
			}
		}
		if err := sc.Err(); err != nil {
			errs <- err
			return
		}
		errs <- nil
	}()
	return lines, errs
}

func sendShell(send streamSender, cmdStr string, pty bool) error {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return fmt.Errorf("shell: command is required")
	}
	argsJSON := buildShellArgsJSON(cmdStr, "", map[string]string{}, pty)
	return send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
		Name:     "shell",
		ArgsJson: argsJSON,
	}}})
}

type chatShell struct {
	name       string
	client     xrunnerv1.RemoteShellServiceClient
	files      xrunnerv1.RemoteFileServiceClient
	printMu    *sync.Mutex
	shellQuiet bool

	mu        sync.Mutex
	byID      map[string]*xrunnerv1.ShellCommandRecord
	order     []string
	waiters   map[string]chan *xrunnerv1.ShellCommandRecord
	attach    xrunnerv1.RemoteShellService_AttachShellClient
	clientID  string
	lastAck   uint64
	compress  xrunnerv1.ShellClientHello_Compression
	recvOnce  sync.Once
	recvErr   error
	recvErrMu sync.Mutex
}

func newChatShell(ctx context.Context, shellc xrunnerv1.RemoteShellServiceClient, filesc xrunnerv1.RemoteFileServiceClient, name string, cols uint32, rows uint32, printMu *sync.Mutex) (*chatShell, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("shell name is required")
	}
	if _, err := shellc.EnsureShellSession(ctx, &xrunnerv1.EnsureShellSessionRequest{Name: name, Cols: cols, Rows: rows}); err != nil {
		return nil, err
	}
	attach, err := shellc.AttachShell(ctx)
	if err != nil {
		return nil, err
	}
	hn, _ := os.Hostname()
	hn = strings.TrimSpace(hn)
	clientID := "chat:" + hn + ":" + name
	if hn == "" {
		clientID = "chat:" + name
	}
	hello := &xrunnerv1.ShellClientHello{
		Name:              name,
		AfterOffset:       0,
		ClientId:          clientID,
		ResumeFromLastAck: true,
		MaxChunkBytes:     0,
		PollMs:            0,
		Compression:       xrunnerv1.ShellClientHello_COMPRESSION_NONE,
	}
	if err := attach.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: hello}}); err != nil {
		return nil, err
	}
	sh := &chatShell{
		name:     name,
		client:   shellc,
		files:    filesc,
		printMu:  printMu,
		byID:     make(map[string]*xrunnerv1.ShellCommandRecord),
		waiters:  make(map[string]chan *xrunnerv1.ShellCommandRecord),
		attach:   attach,
		clientID: clientID,
		compress: hello.GetCompression(),
	}
	sh.recvLoop()
	return sh, nil
}

func (s *chatShell) recvLoop() {
	s.recvOnce.Do(func() {
		go func() {
			for {
				msg, err := s.attach.Recv()
				if err != nil {
					s.recvErrMu.Lock()
					s.recvErr = err
					s.recvErrMu.Unlock()
					return
				}
				if h := msg.GetHello(); h != nil {
					// nothing
					continue
				}
				if ch := msg.GetChunk(); ch != nil && len(ch.GetData()) > 0 {
					u := uint64(ch.GetUncompressedLen())
					if u == 0 {
						u = uint64(len(ch.GetData()))
					}
					s.lastAck = ch.GetOffset() + u
					_ = s.attach.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
						Name:      s.name,
						ClientId:  s.clientID,
						AckOffset: s.lastAck,
					}}})
					continue
				}
				if ce := msg.GetCommand(); ce != nil && ce.GetRecord() != nil {
					rec := ce.GetRecord()
					s.mu.Lock()
					s.byID[rec.GetCommandId()] = rec
					if ce.GetPhase() == xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED {
						s.order = append(s.order, rec.GetCommandId())
						if len(s.order) > 500 {
							s.order = s.order[len(s.order)-500:]
						}
						if w := s.waiters[rec.GetCommandId()]; w != nil {
							delete(s.waiters, rec.GetCommandId())
							select {
							case w <- rec:
							default:
							}
							close(w)
						}
						// Print compact completion line (best-effort).
						arts := len(rec.GetArtifactIds())
						dur := ""
						if rec.GetStartedAt() != nil && rec.GetCompletedAt() != nil {
							dur = rec.GetCompletedAt().AsTime().Sub(rec.GetStartedAt().AsTime()).Truncate(time.Millisecond).String()
						}
						if s.printMu != nil {
							s.printMu.Lock()
							defer s.printMu.Unlock()
						}
						fmt.Fprintf(os.Stderr, "\ncmd done exit=%d dur=%s artifacts=%d id=%s\n", rec.GetExitCode(), dur, arts, rec.GetCommandId())
						if arts > 0 {
							s.printArtifactSummary(rec.GetCommandId(), rec.GetArtifactIds())
							fmt.Fprintf(os.Stderr, "hint: /artifacts %s | /replay %s\n", rec.GetCommandId(), rec.GetCommandId())
						}
					}
					s.mu.Unlock()
				}
			}
		}()
	})
}

type goTestFailureArtifactPayload struct {
	Packages []string `json:"packages"`
	Tests    []string `json:"tests"`
	Files    []struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Text string `json:"text"`
	} `json:"files"`
}

func (s *chatShell) printArtifactSummary(commandID string, artifactIDs []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := s.client.ListShellArtifacts(ctx, &xrunnerv1.ListShellArtifactsRequest{Name: s.name, CommandId: commandID, Limit: 50})
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifacts: %v\n", err)
		return
	}
	for _, a := range resp.GetArtifacts() {
		if a == nil {
			continue
		}
		switch a.GetType() {
		case "go_test_failure":
			var p goTestFailureArtifactPayload
			_ = json.Unmarshal(a.GetDataJson(), &p)
			loc := ""
			if len(p.Files) > 0 {
				loc = fmt.Sprintf(" %s:%d", p.Files[0].File, p.Files[0].Line)
			}
			tname := ""
			if len(p.Tests) > 0 {
				tname = p.Tests[0]
			}
			pkg := ""
			if len(p.Packages) > 0 {
				pkg = p.Packages[0]
			}
			fmt.Fprintf(os.Stderr, "artifact go_test_failure: %s test=%s pkg=%s%s\n", a.GetTitle(), tname, pkg, loc)
		default:
			fmt.Fprintf(os.Stderr, "artifact %s: %s\n", a.GetType(), a.GetTitle())
		}
	}
	_ = artifactIDs
}

func (s *chatShell) getGoTestFailure(ctx context.Context, commandID string) (*goTestFailureArtifactPayload, error) {
	arts, err := s.Artifacts(ctx, commandID, 50)
	if err != nil {
		return nil, err
	}
	for _, a := range arts {
		if a == nil || a.GetType() != "go_test_failure" {
			continue
		}
		var p goTestFailureArtifactPayload
		if err := json.Unmarshal(a.GetDataJson(), &p); err != nil {
			continue
		}
		return &p, nil
	}
	return nil, nil
}

func describeGoTestFailure(p *goTestFailureArtifactPayload) string {
	if p == nil {
		return "<none>"
	}
	t := ""
	if len(p.Tests) > 0 {
		t = p.Tests[0]
	}
	pkg := ""
	if len(p.Packages) > 0 {
		pkg = p.Packages[0]
	}
	loc := ""
	if len(p.Files) > 0 {
		loc = fmt.Sprintf("%s:%d", p.Files[0].File, p.Files[0].Line)
	}
	parts := []string{}
	if t != "" {
		parts = append(parts, "test="+t)
	}
	if pkg != "" {
		parts = append(parts, "pkg="+pkg)
	}
	if loc != "" {
		parts = append(parts, "at="+loc)
	}
	if len(parts) == 0 {
		return "<unknown>"
	}
	return strings.Join(parts, " ")
}

func (s *chatShell) buildRerunFailingCommand(p *goTestFailureArtifactPayload) (string, string) {
	_ = s
	if p == nil {
		return "", ""
	}
	pkg := ""
	if len(p.Packages) > 0 {
		pkg = strings.TrimSpace(p.Packages[0])
	}
	test := ""
	if len(p.Tests) > 0 {
		test = strings.TrimSpace(p.Tests[0])
	}
	cmdStr := "go test ./..."
	if pkg != "" {
		cmdStr = "go test " + pkg
	}
	if test != "" {
		cmdStr += " -run '^" + test + "$' -count=1"
	} else {
		cmdStr += " -count=1"
	}
	return strings.ReplaceAll(uuid.NewString(), "-", ""), cmdStr
}

func (s *chatShell) rerunFailing(ctx context.Context, p *goTestFailureArtifactPayload, timeout time.Duration) (*xrunnerv1.ShellCommandRecord, string) {
	_, cmdStr := s.buildRerunFailingCommand(p)
	if cmdStr == "" {
		return nil, ""
	}
	rec, err := s.Run(ctx, cmdStr, timeout)
	if err != nil {
		return nil, cmdStr
	}
	return rec, cmdStr
}

func (s *chatShell) exitOf(id string) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.byID[id]; r != nil {
		return r.GetExitCode()
	}
	return 0
}

func (s *chatShell) tryHeuristicFix(ctx context.Context, path string, line int, msg string) (bool, string, error) {
	if s.files == nil {
		return false, "fix: remote file service not available", nil
	}
	exp, got, ok := parseExpectedGot(msg)
	if !ok {
		return false, "fix: could not parse expected/got (use /open and fix manually)", nil
	}
	rf, err := s.files.ReadFile(ctx, &xrunnerv1.ReadFileRequest{Path: path})
	if err != nil {
		return false, "", err
	}
	lines := strings.Split(string(rf.GetData()), "\n")
	idx := line - 1
	if idx < 0 || idx >= len(lines) {
		return false, "fix: line out of range", nil
	}
	orig := lines[idx]
	repl, ok := replaceLiteral(orig, exp, got)
	if !ok || repl == orig {
		return false, "fix: could not apply heuristic patch (use /open and fix manually)", nil
	}
	lines[idx] = repl
	next := strings.Join(lines, "\n")
	if _, err := s.files.WriteFileAtomic(ctx, &xrunnerv1.WriteFileRequest{Path: path, Data: []byte(next), Mode: 0o644}); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("fix: patched %s:%d\n- %s\n+ %s", path, line, orig, repl), nil
}

func parseExpectedGot(s string) (expected string, got string, ok bool) {
	s = strings.TrimSpace(s)
	l := strings.ToLower(s)
	i := strings.Index(l, "expected ")
	if i < 0 {
		return "", "", false
	}
	rest := s[i+len("expected "):]
	j := strings.Index(strings.ToLower(rest), ", got ")
	if j < 0 {
		return "", "", false
	}
	expected = strings.TrimSpace(rest[:j])
	got = strings.TrimSpace(rest[j+len(", got "):])
	got = strings.TrimRight(got, ".")
	return expected, got, expected != "" && got != ""
}

func replaceLiteral(line string, expected string, got string) (string, bool) {
	if expected == got {
		return line, false
	}
	if strings.HasPrefix(expected, "\"") && strings.HasSuffix(expected, "\"") {
		if strings.Contains(line, expected) {
			return strings.Replace(line, expected, got, 1), true
		}
	}
	if isDigits(expected) && isDigits(got) {
		for _, sep := range []string{" ", "\t", ")", "]", "}", ",", ";"} {
			if strings.Contains(line, expected+sep) {
				return strings.Replace(line, expected+sep, got+sep, 1), true
			}
		}
		if strings.Contains(line, expected) {
			return strings.Replace(line, expected, got, 1), true
		}
	}
	if strings.Contains(line, expected) {
		return strings.Replace(line, expected, got, 1), true
	}
	return line, false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (s *chatShell) Run(ctx context.Context, cmdStr string, timeout time.Duration) (*xrunnerv1.ShellCommandRecord, error) {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return nil, fmt.Errorf("shell: command is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	ch := make(chan *xrunnerv1.ShellCommandRecord, 1)
	s.mu.Lock()
	s.waiters[id] = ch
	s.mu.Unlock()
	if err := s.attach.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
		Name:      s.name,
		Command:   cmdStr,
		CommandId: id,
		TimeoutMs: uint32(timeout.Milliseconds()),
	}}}); err != nil {
		return nil, err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	select {
	case <-tctx.Done():
		return nil, fmt.Errorf("command timed out waiting for completion event")
	case rec := <-ch:
		if rec == nil {
			return nil, fmt.Errorf("command did not return a record")
		}
		return rec, nil
	}
}

func (s *chatShell) Timeline(ctx context.Context, n int) []*xrunnerv1.ShellCommandRecord {
	if n <= 0 {
		n = 10
	}
	// Prefer durable list from server for correctness.
	resp, err := s.client.ListShellCommands(ctx, &xrunnerv1.ListShellCommandsRequest{Name: s.name, Limit: uint32(n)})
	if err == nil && len(resp.GetCommands()) > 0 {
		return resp.GetCommands()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*xrunnerv1.ShellCommandRecord, 0, n)
	for i := len(s.order) - 1; i >= 0 && len(out) < n; i-- {
		if rec := s.byID[s.order[i]]; rec != nil {
			out = append(out, rec)
		}
	}
	return out
}

func (s *chatShell) findRecord(ctx context.Context, id string) (*xrunnerv1.ShellCommandRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("command id is required")
	}
	s.mu.Lock()
	rec := s.byID[id]
	s.mu.Unlock()
	if rec != nil {
		return rec, nil
	}
	resp, err := s.client.ListShellCommands(ctx, &xrunnerv1.ListShellCommandsRequest{Name: s.name, Limit: 500})
	if err != nil {
		return nil, err
	}
	for _, r := range resp.GetCommands() {
		if r != nil && r.GetCommandId() == id {
			s.mu.Lock()
			s.byID[id] = r
			s.mu.Unlock()
			return r, nil
		}
	}
	return nil, fmt.Errorf("command not found: %s", id)
}

func (s *chatShell) Replay(ctx context.Context, id string, w io.Writer) error {
	rec, err := s.findRecord(ctx, id)
	if err != nil {
		return err
	}
	if rec.GetEndOffset() <= rec.GetBeginOffset() {
		return nil
	}
	attach, err := s.client.AttachShell(ctx)
	if err != nil {
		return err
	}
	cid := "chat-replay:" + s.name
	if err := attach.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
		Name:              s.name,
		AfterOffset:       rec.GetBeginOffset(),
		ClientId:          cid,
		ResumeFromLastAck: false,
		MaxChunkBytes:     0,
		PollMs:            0,
		Compression:       xrunnerv1.ShellClientHello_COMPRESSION_NONE,
	}}}); err != nil {
		return err
	}
	// Wait for hello.
	for {
		msg, err := attach.Recv()
		if err != nil {
			return err
		}
		if msg.GetHello() != nil {
			break
		}
	}
	end := rec.GetEndOffset()
	for {
		msg, err := attach.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ch := msg.GetChunk()
		if ch == nil || len(ch.GetData()) == 0 {
			continue
		}
		start := ch.GetOffset()
		adv := uint64(ch.GetUncompressedLen())
		if adv == 0 {
			adv = uint64(len(ch.GetData()))
		}
		if start >= end {
			return nil
		}
		_, _ = w.Write(ch.GetData())
		_ = attach.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
			Name:      s.name,
			ClientId:  cid,
			AckOffset: start + adv,
		}}})
		if start+adv >= end {
			return nil
		}
	}
}

func (s *chatShell) Rerun(ctx context.Context, id string, timeout time.Duration) (*xrunnerv1.ShellCommandRecord, error) {
	rec, err := s.findRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.Run(ctx, rec.GetCommand(), timeout)
}

func (s *chatShell) Artifacts(ctx context.Context, commandID string, limit int) ([]*xrunnerv1.ShellArtifact, error) {
	if limit <= 0 {
		limit = 50
	}
	resp, err := s.client.ListShellArtifacts(ctx, &xrunnerv1.ListShellArtifactsRequest{
		Name:      s.name,
		CommandId: strings.TrimSpace(commandID),
		Limit:     uint32(limit),
	})
	if err != nil {
		return nil, err
	}
	return resp.GetArtifacts(), nil
}

func (s *chatShell) OpenLocal(ctx context.Context, spec string) error {
	// spec: path:line
	path := spec
	line := 0
	if i := strings.LastIndex(spec, ":"); i > 0 {
		if n, err := strconv.Atoi(spec[i+1:]); err == nil {
			path = spec[:i]
			line = n
		}
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("open: path is required")
	}
	if s.files == nil {
		return fmt.Errorf("open: remote file service not available")
	}
	rf, err := s.files.ReadFile(ctx, &xrunnerv1.ReadFileRequest{Path: path})
	if err != nil {
		return err
	}
	dir := filepath.Join(os.TempDir(), "xrunner-open", s.name)
	dst := filepath.Join(dir, filepath.FromSlash(strings.TrimLeft(path, "/")))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, rf.GetData(), 0o644); err != nil {
		return err
	}
	return openEditor(dst, line)
}

func openEditor(path string, line int) error {
	if _, err := exec.LookPath("code"); err == nil && line > 0 {
		return exec.Command("code", "-g", fmt.Sprintf("%s:%d:1", path, line)).Start()
	}
	if _, err := exec.LookPath("code"); err == nil {
		return exec.Command("code", path).Start()
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", "", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
