package remote_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/control/jobsvc"
	"github.com/antonkrylov/xrunner/internal/control/store"
	"github.com/antonkrylov/xrunner/internal/remote"
)

func TestSessionInteract_ShellAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
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

	// Remote daemon.
	remoteSrv, err := remote.New(remote.Config{
		ListenAddr:    "127.0.0.1:0",
		UpstreamAddr:  upLis.Addr().String(),
		WorkspaceRoot: t.TempDir(),
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
	created, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{WorkspaceRoot: "/"})
	if err != nil {
		t.Fatal(err)
	}
	sid := created.GetSession().GetSessionId()

	stream, err := ss.Interact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{
		SessionId: sid,
	}}}); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]any{"cmd": "for i in $(seq 1 2000); do echo xxxxxxxxxxxxxxxxx; done; sleep 0.2; echo done", "pty": false})
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
		Name:     "shell",
		ArgsJson: string(args),
	}}}); err != nil {
		t.Fatal(err)
	}

	userSentAt := time.Now()
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{
		Text: "ping",
	}}}); err != nil {
		t.Fatal(err)
	}

	var sawAnyOutput, sawResult, sawPing bool
	var outChunks int
	for start := time.Now(); time.Since(start) < 10*time.Second; {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_ToolOutputChunk:
			outChunks++
			sawAnyOutput = true
		case *xrunnerv1.AgentEvent_ToolCallResult:
			sawResult = true
		case *xrunnerv1.AgentEvent_UserMessage:
			if k.UserMessage.GetText() == "ping" {
				sawPing = true
				if time.Since(userSentAt) > 2*time.Second {
					t.Fatalf("user message delivery too slow: %s", time.Since(userSentAt))
				}
			}
		}
		if sawAnyOutput && sawResult && sawPing && outChunks > 100 {
			break
		}
	}
	if !sawAnyOutput {
		t.Fatalf("expected shell output")
	}
	if !sawResult {
		t.Fatalf("expected tool result")
	}
	if !sawPing {
		t.Fatalf("expected ping user message")
	}

	// Replay should include session_started at minimum.
	evs, err := ss.StreamEvents(ctx, &xrunnerv1.StreamEventsRequest{SessionId: sid})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for got < 2 {
		_, err := evs.Recv()
		if err != nil {
			break
		}
		got++
	}
	if got == 0 {
		t.Fatalf("expected replay events")
	}
}
