package remote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/remote"
)

type noopJobService struct {
	xrunnerv1.UnimplementedJobServiceServer
}

func TestRemoteShellService_TmuxBacked(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Upstream JobService (remote daemon needs to dial it on start).
	upLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	xrunnerv1.RegisterJobServiceServer(s, &noopJobService{})
	go func() { _ = s.Serve(upLis) }()
	t.Cleanup(s.Stop)

	tmp := t.TempDir()
	remoteSrv, err := remote.New(remote.Config{
		ListenAddr:    "127.0.0.1:0",
		UpstreamAddr:  upLis.Addr().String(),
		WorkspaceRoot: tmp,
		UnsafeRootFS:  false,
		SessionsDir:   tmp,
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
		t.Fatalf("dial remote daemon: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	shellc := xrunnerv1.NewRemoteShellServiceClient(conn)

	name := "test_shell_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(name) > 64 {
		name = name[:64]
	}
	t.Cleanup(func() {
		_, _ = shellc.CloseShell(context.Background(), &xrunnerv1.ShellCloseRequest{Name: name, Kill: true})
	})

	ens, err := shellc.EnsureShellSession(ctx, &xrunnerv1.EnsureShellSessionRequest{Name: name, Shell: "sh"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Shell-hook capture: raw input should produce command events (zsh/bash hooks emit OSC markers).
	{
		cid := "hookclient"
		attachCtx, attachCancel := context.WithTimeout(ctx, 12*time.Second)
		defer attachCancel()
		st, err := shellc.AttachShell(attachCtx)
		if err != nil {
			t.Fatalf("attach hook: %v", err)
		}
		if err := st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       ens.GetLogSize(),
			ClientId:          cid,
			ResumeFromLastAck: false,
			MaxChunkBytes:     0,
			PollMs:            0,
			Compression:       xrunnerv1.ShellClientHello_COMPRESSION_NONE,
		}}}); err != nil {
			t.Fatalf("hello hook: %v", err)
		}
		for {
			msg, err := st.Recv()
			if err != nil {
				t.Fatalf("recv hello hook: %v", err)
			}
			if msg.GetHello() != nil {
				break
			}
		}
		if err := st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Input{Input: &xrunnerv1.ShellClientInput{
			Name: name,
			Data: []byte("echo hook-capture-ok\n"),
		}}}); err != nil {
			t.Fatalf("input hook: %v", err)
		}
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		var gotEvent bool
		for !gotEvent {
			select {
			case <-deadline.C:
				t.Fatalf("timeout waiting for hook command event completion")
			default:
			}
			msg, err := st.Recv()
			if err != nil {
				t.Fatalf("recv hook: %v", err)
			}
			if ce := msg.GetCommand(); ce != nil && ce.GetPhase() == xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED && ce.GetRecord() != nil {
				if strings.Contains(ce.GetRecord().GetCommand(), "hook-capture-ok") {
					gotEvent = true
				}
			}
			if ch := msg.GetChunk(); ch != nil && len(ch.GetData()) > 0 {
				u := ch.GetUncompressedLen()
				if u == 0 {
					u = uint32(len(ch.GetData()))
				}
				_ = st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
					Name:      name,
					ClientId:  cid,
					AckOffset: ch.GetOffset() + uint64(u),
				}}})
			}
		}
		_ = st.CloseSend()
	}

	// AttachShell: bidi stream with ack/resume.
	{
		cid := "testclient"
		attachCtx, attachCancel := context.WithTimeout(ctx, 10*time.Second)
		defer attachCancel()
		st1, err := shellc.AttachShell(attachCtx)
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		if err := st1.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       ens.GetLogSize(),
			ClientId:          cid,
			ResumeFromLastAck: false,
			MaxChunkBytes:     64 * 1024,
			PollMs:            10,
		}}}); err != nil {
			t.Fatalf("hello1: %v", err)
		}
		// Wait for server hello.
		for {
			msg, err := st1.Recv()
			if err != nil {
				t.Fatalf("recv hello1: %v", err)
			}
			if msg.GetHello() != nil {
				break
			}
		}
		if err := st1.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Input{Input: &xrunnerv1.ShellClientInput{
			Name: name,
			Data: []byte("echo attach-ok\n"),
		}}}); err != nil {
			t.Fatalf("input1: %v", err)
		}
		var got1 bytes.Buffer
		deadline := time.NewTimer(6 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-deadline.C:
				t.Fatalf("timeout waiting for attach-ok; got %q", got1.String())
			default:
			}
			msg, err := st1.Recv()
			if err != nil {
				t.Fatalf("recv1: %v", err)
			}
			if ch := msg.GetChunk(); ch != nil && len(ch.GetData()) > 0 {
				got1.Write(ch.GetData())
				_ = st1.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
					Name:      name,
					ClientId:  cid,
					AckOffset: ch.GetOffset() + uint64(len(ch.GetData())),
				}}})
				if bytes.Contains(got1.Bytes(), []byte("attach-ok")) {
					break
				}
			}
		}
		_ = st1.CloseSend()

		attachCtx2, attachCancel2 := context.WithTimeout(ctx, 10*time.Second)
		defer attachCancel2()
		st2, err := shellc.AttachShell(attachCtx2)
		if err != nil {
			t.Fatalf("attach2: %v", err)
		}
		if err := st2.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       0,
			ClientId:          cid,
			ResumeFromLastAck: true,
			MaxChunkBytes:     64 * 1024,
			PollMs:            10,
		}}}); err != nil {
			t.Fatalf("hello2: %v", err)
		}
		for {
			msg, err := st2.Recv()
			if err != nil {
				t.Fatalf("recv hello2: %v", err)
			}
			if msg.GetHello() != nil {
				break
			}
		}
		if err := st2.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Input{Input: &xrunnerv1.ShellClientInput{
			Name: name,
			Data: []byte("echo attach2-ok\n"),
		}}}); err != nil {
			t.Fatalf("input2: %v", err)
		}
		var got2 bytes.Buffer
		deadline2 := time.NewTimer(6 * time.Second)
		defer deadline2.Stop()
		for {
			select {
			case <-deadline2.C:
				t.Fatalf("timeout waiting for attach2-ok; got %q", got2.String())
			default:
			}
			msg, err := st2.Recv()
			if err != nil {
				t.Fatalf("recv2: %v", err)
			}
			if ch := msg.GetChunk(); ch != nil && len(ch.GetData()) > 0 {
				got2.Write(ch.GetData())
				_ = st2.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
					Name:      name,
					ClientId:  cid,
					AckOffset: ch.GetOffset() + uint64(len(ch.GetData())),
				}}})
				if bytes.Contains(got2.Bytes(), []byte("attach2-ok")) {
					break
				}
			}
		}
		_ = st2.CloseSend()
	}

	// AttachShell run -> command event + timeline record.
	{
		cid := "eventclient"
		attachCtx, attachCancel := context.WithTimeout(ctx, 12*time.Second)
		defer attachCancel()
		st, err := shellc.AttachShell(attachCtx)
		if err != nil {
			t.Fatalf("attach event: %v", err)
		}
		if err := st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       0,
			ClientId:          cid,
			ResumeFromLastAck: true,
			MaxChunkBytes:     0,
			PollMs:            0,
			Compression:       xrunnerv1.ShellClientHello_COMPRESSION_NONE,
		}}}); err != nil {
			t.Fatalf("hello event: %v", err)
		}
		for {
			msg, err := st.Recv()
			if err != nil {
				t.Fatalf("recv hello event: %v", err)
			}
			if msg.GetHello() != nil {
				break
			}
		}
		if err := st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
			Name:    name,
			Command: "echo cmd-event-ok",
		}}}); err != nil {
			t.Fatalf("run event: %v", err)
		}
		var gotCompleted bool
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		for !gotCompleted {
			select {
			case <-deadline.C:
				t.Fatalf("timeout waiting for command event completion")
			default:
			}
			msg, err := st.Recv()
			if err != nil {
				t.Fatalf("recv event: %v", err)
			}
			if ce := msg.GetCommand(); ce != nil && ce.GetPhase() == xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED {
				if ce.GetRecord() != nil && strings.Contains(ce.GetRecord().GetCommand(), "cmd-event-ok") {
					gotCompleted = true
				}
			}
		}
		_ = st.CloseSend()

		list, err := shellc.ListShellCommands(ctx, &xrunnerv1.ListShellCommandsRequest{Name: name, Limit: 200})
		if err != nil {
			t.Fatalf("list commands: %v", err)
		}
		var found bool
		for _, r := range list.GetCommands() {
			if r != nil && strings.Contains(r.GetCommand(), "cmd-event-ok") {
				found = true
				if r.GetEndOffset() <= r.GetBeginOffset() {
					t.Fatalf("expected end_offset > begin_offset for recorded command")
				}
				break
			}
		}
		if !found {
			t.Fatalf("expected timeline to include cmd-event-ok")
		}
	}

	// AttachShell with zstd compression.
	{
		dec, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatalf("zstd reader: %v", err)
		}
		defer dec.Close()

		cid := "zstdclient"
		attachCtx, attachCancel := context.WithTimeout(ctx, 10*time.Second)
		defer attachCancel()
		st, err := shellc.AttachShell(attachCtx)
		if err != nil {
			t.Fatalf("attach zstd: %v", err)
		}
		if err := st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       0,
			ClientId:          cid,
			ResumeFromLastAck: true,
			MaxChunkBytes:     0,
			PollMs:            0,
			Compression:       xrunnerv1.ShellClientHello_COMPRESSION_ZSTD,
		}}}); err != nil {
			t.Fatalf("hello zstd: %v", err)
		}
		// Wait for server hello.
		var gotHello bool
		for !gotHello {
			msg, err := st.Recv()
			if err != nil {
				t.Fatalf("recv hello zstd: %v", err)
			}
			if h := msg.GetHello(); h != nil {
				if h.GetCompression() != xrunnerv1.ShellClientHello_COMPRESSION_ZSTD {
					t.Fatalf("expected zstd hello, got %v", h.GetCompression())
				}
				gotHello = true
			}
		}
		if err := st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Input{Input: &xrunnerv1.ShellClientInput{
			Name: name,
			Data: []byte("echo zstd-ok\n"),
		}}}); err != nil {
			t.Fatalf("input zstd: %v", err)
		}
		var out bytes.Buffer
		deadline := time.NewTimer(6 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-deadline.C:
				t.Fatalf("timeout waiting for zstd-ok; got %q", out.String())
			default:
			}
			msg, err := st.Recv()
			if err != nil {
				t.Fatalf("recv zstd: %v", err)
			}
			ch := msg.GetChunk()
			if ch == nil || len(ch.GetData()) == 0 {
				continue
			}
			plain, derr := dec.DecodeAll(ch.GetData(), nil)
			if derr != nil {
				t.Fatalf("decode zstd: %v", derr)
			}
			out.Write(plain)
			_ = st.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
				Name:      name,
				ClientId:  cid,
				AckOffset: ch.GetOffset() + uint64(ch.GetUncompressedLen()),
			}}})
			if bytes.Contains(out.Bytes(), []byte("zstd-ok")) {
				break
			}
		}
		_ = st.CloseSend()
	}

	// Stream new output from the current end, then write a command and observe it.
	streamCtx, streamCancel := context.WithTimeout(ctx, 10*time.Second)
	defer streamCancel()
	st, err := shellc.StreamShell(streamCtx, &xrunnerv1.ShellStreamRequest{Name: name, Offset: ens.GetLogSize(), Follow: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if _, err := shellc.WriteShell(ctx, &xrunnerv1.ShellWriteRequest{Name: name, Data: []byte("echo shell-ok\n")}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got bytes.Buffer
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		got.Write(ch.GetData())
		if bytes.Contains(got.Bytes(), []byte("shell-ok")) {
			break
		}
	}

	// RunShell waits for completion + provides log offsets.
	run, err := shellc.RunShell(ctx, &xrunnerv1.ShellRunRequest{Name: name, Command: "echo run-ok", TimeoutMs: 8000})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.GetExitCode() != 0 {
		t.Fatalf("expected exit 0, got %d", run.GetExitCode())
	}
	if run.GetEndOffset() <= run.GetBeginOffset() {
		t.Fatalf("expected end_offset > begin_offset (got %d <= %d)", run.GetEndOffset(), run.GetBeginOffset())
	}

	rangeCtx, rangeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer rangeCancel()
	rs, err := shellc.StreamShell(rangeCtx, &xrunnerv1.ShellStreamRequest{Name: name, Offset: run.GetBeginOffset(), Follow: true})
	if err != nil {
		t.Fatalf("stream range: %v", err)
	}
	var out bytes.Buffer
	for out.Len() < int(run.GetEndOffset()-run.GetBeginOffset()) {
		ch, err := rs.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("range recv: %v", err)
		}
		if len(ch.GetData()) == 0 {
			continue
		}
		start := ch.GetOffset()
		end := start + uint64(len(ch.GetData()))
		if start >= run.GetEndOffset() {
			break
		}
		data := ch.GetData()
		if end > run.GetEndOffset() {
			data = data[:run.GetEndOffset()-start]
		}
		out.Write(data)
		if bytes.Contains(out.Bytes(), []byte("run-ok")) {
			break
		}
	}
	if !bytes.Contains(out.Bytes(), []byte("run-ok")) {
		t.Fatalf("expected run output to contain run-ok; got %q", out.String())
	}

	// TailShell provides a bounded replay window.
	tail, err := shellc.TailShell(ctx, &xrunnerv1.ShellTailRequest{Name: name, Lines: 50})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if !bytes.Contains(tail.GetData(), []byte("shell-ok")) {
		t.Fatalf("expected tail to contain shell-ok; got %q", string(tail.GetData()))
	}
}
