package remote_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/control/jobsvc"
	"github.com/antonkrylov/xrunner/internal/control/store"
	"github.com/antonkrylov/xrunner/internal/remote"
)

type fakeWorker struct {
	xrunnerv1.UnimplementedWorkerServiceServer
}

func (w *fakeWorker) RunJob(req *xrunnerv1.JobAssignment, stream xrunnerv1.WorkerService_RunJobServer) error {
	ref := req.GetRef()
	now := time.Now()
	// send a couple log chunks, then terminal response.
	_ = stream.Send(&xrunnerv1.RunJobResponse{
		Ref:       ref,
		StartedAt: timestamppb.New(now),
		Logs: []*xrunnerv1.LogChunk{
			{Timestamp: timestamppb.New(now), Stream: "stdout", Data: []byte("hello\n")},
		},
	})
	time.Sleep(100 * time.Millisecond)
	_ = stream.Send(&xrunnerv1.RunJobResponse{
		Ref:         ref,
		Logs:        []*xrunnerv1.LogChunk{{Timestamp: timestamppb.New(time.Now()), Stream: "stdout", Data: []byte("world\n")}},
		Terminal:    true,
		FinalState:  xrunnerv1.JobState_JOB_STATE_SUCCEEDED,
		ExitCode:    0,
		CompletedAt: timestamppb.New(time.Now()),
	})
	return nil
}

func startGRPCServer(t *testing.T, register func(*grpc.Server), lis net.Listener) *grpc.Server {
	t.Helper()
	s := grpc.NewServer()
	register(s)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return s
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestRemoteDaemon_Phase1(t *testing.T) {
	// Keep this integration test hermetic: it should not depend on the developer's
	// shell env (OPENAI_*/XR_LLM_*) and should never hit external networks.
	t.Setenv("XR_LLM_ENABLED", "0")
	t.Setenv("XR_LLM_ENABLE", "0")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	// Upstream JobService.
	st := store.MustNew()
	jobService := jobsvc.New(st, workerClient, nil)
	jobService.SetLogHeartbeatInterval(50 * time.Millisecond)

	upLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	startGRPCServer(t, func(s *grpc.Server) {
		xrunnerv1.RegisterJobServiceServer(s, jobService)
	}, upLis)

	// Remote daemon proxying upstream.
	tmp := t.TempDir()
	remoteSrv, err := remote.New(remote.Config{
		ListenAddr:    "127.0.0.1:0",
		UpstreamAddr:  upLis.Addr().String(),
		WorkspaceRoot: tmp,
		UnsafeRootFS:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteSrv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remoteSrv.Stop)

	remoteConn := dial(t, remoteSrv.Addr().String())

	// RemoteControlService.
	ctrl := xrunnerv1.NewRemoteControlServiceClient(remoteConn)
	if _, err := ctrl.Ping(ctx, &emptypb.Empty{}); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// RemoteFileService.
	files := xrunnerv1.NewRemoteFileServiceClient(remoteConn)
	if _, err := files.WriteFileAtomic(ctx, &xrunnerv1.WriteFileRequest{
		Path: "hello.txt",
		Data: []byte("hi"),
		Mode: 0o644,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	rf, err := files.ReadFile(ctx, &xrunnerv1.ReadFileRequest{Path: "hello.txt"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(rf.GetData()) != "hi" {
		t.Fatalf("unexpected file content: %q", string(rf.GetData()))
	}
	if _, err := os.Stat(filepath.Join(tmp, "hello.txt")); err != nil {
		t.Fatalf("expected file on disk: %v", err)
	}

	// RemotePTYService.
	if runtime.GOOS != "windows" {
		ptyc := xrunnerv1.NewRemotePTYServiceClient(remoteConn)
		open, err := ptyc.OpenPTY(ctx, &xrunnerv1.OpenPTYRequest{
			Command: "sh",
			Args:    []string{"-lc", "echo pty-ok"},
		})
		if err != nil {
			t.Fatalf("open pty: %v", err)
		}
		stream, err := ptyc.StreamPTY(ctx, &xrunnerv1.PTYStreamRequest{SessionId: open.GetSessionId()})
		if err != nil {
			t.Fatalf("stream pty: %v", err)
		}
		var buf bytes.Buffer
		deadline := time.After(3 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("timeout waiting for pty output; got %q", buf.String())
			default:
			}
			ch, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("pty recv: %v", err)
			}
			buf.Write(ch.GetData())
			if bytes.Contains(buf.Bytes(), []byte("pty-ok")) {
				break
			}
		}
	}

	// JobService proxy.
	jobs := xrunnerv1.NewJobServiceClient(remoteConn)
	sub, err := jobs.SubmitJob(ctx, &xrunnerv1.SubmitJobRequest{
		TenantId: "default",
		Workload: &xrunnerv1.Workload{
			Kind: &xrunnerv1.Workload_Inline{
				Inline: &xrunnerv1.InlineWorkload{
					Executable: "/bin/echo",
					Args:       []string{"hi"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	jobID := sub.GetJob().GetRef().GetJobId()
	if jobID == "" {
		t.Fatalf("expected job id")
	}

	logs, err := jobs.StreamJobLogs(ctx, &xrunnerv1.StreamJobLogsRequest{
		Ref: &xrunnerv1.JobRef{TenantId: "default", JobId: jobID},
	})
	if err != nil {
		t.Fatalf("stream logs: %v", err)
	}
	var gotHello, gotHeartbeat bool
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		ch, err := logs.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("logs recv: %v", err)
		}
		if ch.GetStream() == "heartbeat" {
			gotHeartbeat = true
			continue
		}
		if bytes.Contains(ch.GetData(), []byte("hello")) {
			gotHello = true
		}
		if gotHello && gotHeartbeat {
			break
		}
	}
	if !gotHello {
		t.Fatalf("expected hello in logs")
	}
	if !gotHeartbeat {
		t.Fatalf("expected heartbeat in logs")
	}
}
