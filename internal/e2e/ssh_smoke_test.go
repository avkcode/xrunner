package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

func TestE2E_SSHRemoteHost(t *testing.T) {
	target := strings.TrimSpace(os.Getenv("XRUNNER_E2E_SSH_HOST"))
	if target == "" {
		t.Skip("set XRUNNER_E2E_SSH_HOST (e.g. root@188.124.37.233) to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Build xrunner-remote locally.
	tmp := t.TempDir()
	localBin := filepath.Join(tmp, "xrunner-remote")
	build := exec.CommandContext(ctx, "go", "build", "-o", localBin, "./cmd/xrunner-remote")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build xrunner-remote: %v\n%s", err, out)
	}

	// Install on remote.
	run(t, ctx, "ssh", target, "mkdir -p /root/.xrunner/bin")
	run(t, ctx, "scp", localBin, fmt.Sprintf("%s:/root/.xrunner/bin/xrunner-remote.upload", target))
	run(t, ctx, "ssh", target, "mv /root/.xrunner/bin/xrunner-remote.upload /root/.xrunner/bin/xrunner-remote && chmod +x /root/.xrunner/bin/xrunner-remote")

	// Start daemon (best-effort; if already running, keep it).
	// Use a fixed port 7337 on remote localhost.
	startCmd := strings.Join([]string{
		"nohup /root/.xrunner/bin/xrunner-remote",
		"--listen 127.0.0.1:7337",
		"--upstream 127.0.0.1:50051",
		"--workspace-root /",
		"--unsafe-allow-rootfs=true",
		">/root/.xrunner/remote.log 2>&1 &",
	}, " ")
	run(t, ctx, "ssh", target, startCmd)

	// Local port forward to remote daemon.
	localPort := freeLocalPort(t)
	fwd := exec.CommandContext(ctx, "ssh",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:7337", localPort),
		"-N", target,
	)
	fwd.Stdout = os.Stdout
	fwd.Stderr = os.Stderr
	if err := fwd.Start(); err != nil {
		t.Fatalf("start ssh forward: %v", err)
	}
	t.Cleanup(func() { _ = fwd.Process.Kill() })

	// Wait for forward to be ready.
	waitForTCP(t, ctx, fmt.Sprintf("127.0.0.1:%d", localPort))

	// Dial remote daemon.
	connCtx, connCancel := context.WithTimeout(ctx, 10*time.Second)
	defer connCancel()
	conn, err := grpc.DialContext(connCtx, fmt.Sprintf("127.0.0.1:%d", localPort), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial remote daemon: %v", err)
	}
	defer conn.Close()

	ctrl := xrunnerv1.NewRemoteControlServiceClient(conn)
	if _, err := ctrl.Ping(ctx, &emptypb.Empty{}); err != nil {
		t.Fatalf("ping: %v", err)
	}

	ss := xrunnerv1.NewSessionServiceClient(conn)
	created, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{WorkspaceRoot: "/"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sid := created.GetSession().GetSessionId()
	stdout, _, exit := invokeShell(t, ctx, ss, sid, `echo session-ssh-ok`, false)
	if exit != 0 || !strings.Contains(stdout, "session-ssh-ok") {
		t.Fatalf("expected shell output; exit=%d stdout=%q", exit, stdout)
	}

	ptyc := xrunnerv1.NewRemotePTYServiceClient(conn)
	open, err := ptyc.OpenPTY(ctx, &xrunnerv1.OpenPTYRequest{
		Command: "sh",
		Args:    []string{"-lc", "echo remote-pty-ok"},
	})
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	stream, err := ptyc.StreamPTY(ctx, &xrunnerv1.PTYStreamRequest{SessionId: open.GetSessionId()})
	if err != nil {
		t.Fatalf("stream pty: %v", err)
	}
	var ptyBuf bytes.Buffer
	readDeadline := time.NewTimer(10 * time.Second)
	defer readDeadline.Stop()
	for {
		select {
		case <-readDeadline.C:
			t.Fatalf("timeout waiting for pty output; got %q", ptyBuf.String())
		default:
		}
		ch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("pty recv: %v", err)
		}
		ptyBuf.Write(ch.GetData())
		if strings.Contains(ptyBuf.String(), "remote-pty-ok") {
			break
		}
	}

	// tmux-backed durability primitives (used by xrunner ssh chat --resume / attach --resume).
	stdout, _, exit = invokeShell(t, ctx, ss, sid, `command -v tmux >/dev/null 2>&1 && tmux -V`, false)
	if exit != 0 {
		t.Skipf("tmux not available on remote host; stdout=%q", stdout)
	}
	_, _, _ = invokeShell(t, ctx, ss, sid, `tmux kill-session -t e2e_ops 2>/dev/null || true; tmux new -ds e2e_ops sh; tmux send-keys -t e2e_ops -l 'echo e2e-tmux-ok' C-m; sleep 0.2`, false)
	open, err = ptyc.OpenPTY(ctx, &xrunnerv1.OpenPTYRequest{
		Command: "sh",
		Args:    []string{"-lc", "tmux capture-pane -pt e2e_ops -S -50 -p -J | grep -F e2e-tmux-ok"},
	})
	if err != nil {
		t.Fatalf("open pty (tmux capture): %v", err)
	}
	stream, err = ptyc.StreamPTY(ctx, &xrunnerv1.PTYStreamRequest{SessionId: open.GetSessionId()})
	if err != nil {
		t.Fatalf("stream pty (tmux capture): %v", err)
	}
	ptyBuf.Reset()
	readDeadline.Reset(10 * time.Second)
	for {
		select {
		case <-readDeadline.C:
			t.Fatalf("timeout waiting for tmux capture output; got %q", ptyBuf.String())
		default:
		}
		ch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("pty recv (tmux capture): %v", err)
		}
		ptyBuf.Write(ch.GetData())
		if strings.Contains(ptyBuf.String(), "e2e-tmux-ok") {
			break
		}
	}

	// Basic job list to confirm JobService proxy works.
	jobs := xrunnerv1.NewJobServiceClient(conn)
	if _, err := jobs.ListJobs(ctx, &xrunnerv1.ListJobsRequest{TenantId: "default"}); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
}

func run(t *testing.T, ctx context.Context, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (go.mod) from %s", dir)
		}
		dir = parent
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForTCP(t *testing.T, ctx context.Context, addr string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for tcp %s", addr)
		case <-ticker.C:
			c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				return
			}
		}
	}
}

func invokeShell(t *testing.T, ctx context.Context, ss xrunnerv1.SessionServiceClient, sessionID string, cmd string, pty bool) (stdout string, stderr string, exit int32) {
	t.Helper()
	chat, err := ss.Interact(ctx)
	if err != nil {
		t.Fatalf("interact: %v", err)
	}
	if err := chat.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{SessionId: sessionID}}}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	args := fmt.Sprintf(`{"cmd":%q,"pty":%v}`, cmd, pty)
	if err := chat.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
		Name:     "shell",
		ArgsJson: args,
	}}}); err != nil {
		t.Fatalf("tool invoke: %v", err)
	}

	var toolID string
	var outB bytes.Buffer
	var errB bytes.Buffer
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("timeout waiting for shell tool result; stdout=%q stderr=%q", outB.String(), errB.String())
		default:
		}
		ev, err := chat.Recv()
		if err != nil {
			t.Fatalf("session recv: %v", err)
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
			streamName := strings.ToLower(strings.TrimSpace(k.ToolOutputChunk.GetStream()))
			if streamName == "stderr" {
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
			return outB.String(), errB.String(), k.ToolCallResult.GetExitCode()
		}
	}
}
