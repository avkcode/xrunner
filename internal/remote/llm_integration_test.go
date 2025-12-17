package remote_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/control/jobsvc"
	"github.com/antonkrylov/xrunner/internal/control/store"
	"github.com/antonkrylov/xrunner/internal/remote"
)

func TestSessionInteract_LLMToolLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	var reqN int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		n := atomic.AddInt32(&reqN, 1)
		switch n {
		case 1:
			// Ask the server to run the shell tool.
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_0",
									"type": "function",
									"function": map[string]any{
										"name":      "shell",
										"arguments": `{"cmd":"echo hi","pty":false}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "done",
						},
						"finish_reason": "stop",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	defer llm.Close()

	t.Setenv("XR_LLM_ENABLED", "1")
	t.Setenv("XR_LLM_PROVIDER", "openai_compat")
	t.Setenv("XR_LLM_BASE_URL", llm.URL)
	t.Setenv("XR_LLM_CHAT_PATH", "/v1/chat/completions")
	t.Setenv("XR_LLM_API_KEY", "test-key")
	t.Setenv("XR_LLM_MODEL", "test-model")
	t.Setenv("XR_LLM_STREAM", "0")

	// Minimal tool bundle that allows shell tool invocation.
	toolBundle := filepath.Join(t.TempDir(), "bundle.json")
	bundleJSON := `{
  "schema_version": "2025-12-16",
  "bundle_version": "v2025-12-16",
  "generated_at": "2025-12-16T00:00:00Z",
  "tools": [
    {
      "type": "function",
      "strict": true,
      "function": {
        "name": "shell",
        "description": "Run a shell command inside the session workspace and return output.",
        "parameters": {
          "type": "object",
          "properties": {
            "cmd": { "type": "string" },
            "cwd": { "type": "string" },
            "env": { "type": "object", "additionalProperties": { "type": "string" } },
            "pty": { "type": "boolean" }
          },
          "required": ["cmd"],
          "additionalProperties": false
        }
      }
    }
  ]
}`
	if err := os.WriteFile(toolBundle, []byte(bundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}

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
	created, err := ss.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{
		WorkspaceRoot:  "/",
		ToolBundlePath: toolBundle,
	})
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

	evCh := make(chan *xrunnerv1.AgentEvent, 256)
	recvErrCh := make(chan error, 1)
	go func() {
		defer close(evCh)
		for {
			ev, err := stream.Recv()
			if err != nil {
				recvErrCh <- err
				return
			}
			evCh <- ev
		}
	}()

	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{
		Text: "run shell",
	}}}); err != nil {
		t.Fatal(err)
	}

	var sawToolStart, sawToolResult, sawDone bool
	for start := time.Now(); time.Since(start) < 10*time.Second; {
		var ev *xrunnerv1.AgentEvent
		select {
		case err := <-recvErrCh:
			if err == io.EOF {
				ev = nil
			} else {
				t.Fatal(err)
			}
		case ev = <-evCh:
			if ev == nil {
				continue
			}
		case <-time.After(250 * time.Millisecond):
			continue
		}
		if ev == nil {
			break
		}
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_ToolCallStarted:
			if k.ToolCallStarted.GetName() == "shell" {
				sawToolStart = true
			}
		case *xrunnerv1.AgentEvent_ToolCallResult:
			if k.ToolCallResult.GetExitCode() == 0 {
				sawToolResult = true
			}
		case *xrunnerv1.AgentEvent_AssistantMessage:
			if strings.Contains(k.AssistantMessage.GetText(), "done") {
				sawDone = true
			}
		}
		if sawToolStart && sawToolResult && sawDone {
			break
		}
	}
	if !sawToolStart {
		t.Fatalf("expected tool start")
	}
	if !sawToolResult {
		t.Fatalf("expected tool result exit=0")
	}
	if !sawDone {
		t.Fatalf("expected assistant done message")
	}
}
