package remote_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestSessionInteract_LLMStreamingAssistantChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"},\"index\":0}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"index\":0}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer llm.Close()

	t.Setenv("XR_LLM_ENABLED", "1")
	t.Setenv("XR_LLM_PROVIDER", "openai_compat")
	t.Setenv("XR_LLM_BASE_URL", llm.URL)
	t.Setenv("XR_LLM_CHAT_PATH", "/v1/chat/completions")
	t.Setenv("XR_LLM_API_KEY", "test-key")
	t.Setenv("XR_LLM_MODEL", "test-model")
	t.Setenv("XR_LLM_STREAM", "1")
	t.Setenv("XR_LLM_MAX_TOOL_ITERS", "1")

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
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{SessionId: sid}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: "say hello"}}}); err != nil {
		t.Fatal(err)
	}

	var got strings.Builder
	sawChunk := false
	sawAssistantMsg := false
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_ToolOutputChunk:
			if strings.EqualFold(k.ToolOutputChunk.GetStream(), "assistant") {
				sawChunk = true
				got.Write(k.ToolOutputChunk.GetData())
			}
		case *xrunnerv1.AgentEvent_AssistantMessage:
			sawAssistantMsg = true
		case *xrunnerv1.AgentEvent_Status:
			if k.Status.GetPhase() == "idle" && sawChunk {
				goto done
			}
		}
	}
done:
	if !sawChunk {
		t.Fatalf("expected assistant ToolOutputChunk stream")
	}
	if sawAssistantMsg {
		t.Fatalf("did not expect AssistantMessage broadcast when streaming is enabled")
	}
	if strings.TrimSpace(got.String()) != "hello world" {
		t.Fatalf("got=%q", got.String())
	}
}
