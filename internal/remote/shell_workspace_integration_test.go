package remote_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/control/jobsvc"
	"github.com/antonkrylov/xrunner/internal/control/store"
	"github.com/antonkrylov/xrunner/internal/remote"
)

func TestSessionInteract_ShellCwdIsWorkspaceRelative(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Worker.
	workerLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	startGRPCServer(t, func(s *grpc.Server) {
		xrunnerv1.RegisterWorkerServiceServer(s, &fakeWorker{})
	}, workerLis)
	workerConn := dial(t, workerLis.Addr().String())
	workerClient := xrunnerv1.NewWorkerServiceClient(workerConn)

	// Upstream JobService (required by remote daemon).
	st := store.MustNew()
	jobService := jobsvc.New(st, workerClient, nil)
	jobService.SetLogHeartbeatInterval(0)
	upLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	startGRPCServer(t, func(s *grpc.Server) {
		xrunnerv1.RegisterJobServiceServer(s, jobService)
	}, upLis)

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Remote daemon.
	remoteSrv, err := remote.New(remote.Config{
		ListenAddr:    "127.0.0.1:0",
		UpstreamAddr:  upLis.Addr().String(),
		WorkspaceRoot: ws,
		UnsafeRootFS:  false,
		SessionsDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteSrv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remoteSrv.Stop)

	connCtx, connCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connCancel()
	conn, err := grpc.DialContext(connCtx, remoteSrv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ss := xrunnerv1.NewSessionServiceClient(conn)
	created, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{WorkspaceRoot: ws})
	if err != nil {
		t.Fatal(err)
	}
	sid := created.GetSession().GetSessionId()

	stream, err := ss.Interact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{SessionId: sid}}}); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]any{"cmd": "pwd", "cwd": "sub", "pty": false})
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
		Name:     "shell",
		ArgsJson: string(args),
	}}}); err != nil {
		t.Fatal(err)
	}

	var got strings.Builder
	sawResult := false
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_ToolOutputChunk:
			if strings.EqualFold(k.ToolOutputChunk.GetStream(), "stdout") {
				got.Write(k.ToolOutputChunk.GetData())
			}
		case *xrunnerv1.AgentEvent_ToolCallResult:
			sawResult = true
			if k.ToolCallResult.GetExitCode() != 0 {
				t.Fatalf("exit=%d err=%s", k.ToolCallResult.GetExitCode(), k.ToolCallResult.GetErrorMessage())
			}
		}
		if sawResult && strings.TrimSpace(got.String()) != "" {
			break
		}
	}
	want := filepath.Join(ws, "sub")
	if !strings.Contains(strings.TrimSpace(got.String()), want) {
		t.Fatalf("got=%q wantContains=%q", got.String(), want)
	}

	// Now ensure escaping the workspace root is rejected.
	args, _ = json.Marshal(map[string]any{"cmd": "pwd", "cwd": "../", "pty": false})
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
		Name:     "shell",
		ArgsJson: string(args),
	}}}); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if k := ev.GetToolCallResult(); k != nil {
			if k.GetExitCode() == 0 {
				t.Fatalf("expected non-zero exit for escaping cwd")
			}
			if !strings.Contains(strings.ToLower(k.GetErrorMessage()), "cwd") {
				t.Fatalf("err=%q", k.GetErrorMessage())
			}
			return
		}
	}
	t.Fatalf("expected ToolCallResult for escaping cwd")
}
