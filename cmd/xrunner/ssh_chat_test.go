package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type fakeInteractClient struct {
	sent []*xrunnerv1.ClientMsg
}

func (f *fakeInteractClient) Send(msg *xrunnerv1.ClientMsg) error {
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeInteractClient) Recv() (*xrunnerv1.AgentEvent, error) { return nil, nil }
func (f *fakeInteractClient) Header() (metadata.MD, error)         { return metadata.MD{}, nil }
func (f *fakeInteractClient) Trailer() metadata.MD                 { return metadata.MD{} }
func (f *fakeInteractClient) CloseSend() error                     { return nil }
func (f *fakeInteractClient) Context() context.Context             { return context.Background() }
func (f *fakeInteractClient) SendMsg(any) error                    { return nil }
func (f *fakeInteractClient) RecvMsg(any) error                    { return nil }

type fakeSessionClient struct {
	resp *xrunnerv1.ListSessionsResponse
}

func (f *fakeSessionClient) ListSessions(context.Context, *xrunnerv1.ListSessionsRequest, ...grpc.CallOption) (*xrunnerv1.ListSessionsResponse, error) {
	return f.resp, nil
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	b := <-done
	_ = r.Close()
	return string(b)
}

func TestHandleChatCommand_WriteSendsToolInvoke(t *testing.T) {
	ctx := context.Background()
	ss := &fakeSessionClient{}
	stream := &fakeInteractClient{}
	printMu := &sync.Mutex{}

	handled, promptNow, err := handleChatCommand(ctx, ss, stream.Send, "/write foo.txt hello world", true, false, nil, "", nil, 0, printMu)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatalf("expected handled")
	}
	if promptNow {
		t.Fatalf("expected promptNow=false")
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent=%d", len(stream.sent))
	}
	inv := stream.sent[0].GetToolInvoke()
	if inv == nil {
		t.Fatalf("expected ToolInvoke")
	}
	if inv.GetName() != "write_file_atomic" {
		t.Fatalf("tool=%q", inv.GetName())
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(inv.GetArgsJson()), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "foo.txt" {
		t.Fatalf("path=%v", args["path"])
	}
	if args["content"] != "hello world" {
		t.Fatalf("content=%v", args["content"])
	}
}

func TestHandleChatCommand_SessionsPrints(t *testing.T) {
	ctx := context.Background()
	ss := &fakeSessionClient{resp: &xrunnerv1.ListSessionsResponse{
		Sessions: []*xrunnerv1.Session{{SessionId: "s1", WorkspaceRoot: "/", ToolBundlePath: "/tmp/bundle.json"}},
	}}
	stream := &fakeInteractClient{}
	printMu := &sync.Mutex{}

	out := captureStdout(t, func() {
		handled, promptNow, err := handleChatCommand(ctx, ss, stream.Send, "/sessions", true, false, nil, "", nil, 0, printMu)
		if err != nil {
			t.Fatal(err)
		}
		if !handled {
			t.Fatalf("expected handled")
		}
		if !promptNow {
			t.Fatalf("expected promptNow=true")
		}
	})
	if len(stream.sent) != 0 {
		t.Fatalf("expected no sends")
	}
	if !strings.Contains(out, "session s1 workspace=/ tool_bundle=/tmp/bundle.json") {
		t.Fatalf("out=%q", out)
	}
}
