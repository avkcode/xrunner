package remote

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type ptySession struct {
	id     string
	cmd    *exec.Cmd
	f      *os.File
	cancel context.CancelFunc

	endOnce     sync.Once
	cleanupOnce sync.Once
	closed      chan struct{}
}

type ptyService struct {
	xrunnerv1.UnimplementedRemotePTYServiceServer

	mu       sync.Mutex
	sessions map[string]*ptySession
}

func newPTYService() *ptyService {
	return &ptyService{sessions: make(map[string]*ptySession)}
}

func (s *ptyService) OpenPTY(ctx context.Context, req *xrunnerv1.OpenPTYRequest) (*xrunnerv1.OpenPTYResponse, error) {
	if req.GetCommand() == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, req.GetCommand(), req.GetArgs()...)
	if cwd := req.GetCwd(); cwd != "" {
		cmd.Dir = cwd
	}
	if env := req.GetEnv(); len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	ws := &pty.Winsize{Cols: 120, Rows: 30}
	if req.GetCols() > 0 {
		ws.Cols = uint16(req.GetCols())
	}
	if req.GetRows() > 0 {
		ws.Rows = uint16(req.GetRows())
	}

	ptyFile, err := startPTY(cmd, ws, true)
	if err != nil && strings.Contains(err.Error(), "Setctty set but Ctty not valid") {
		// Some platforms/Go versions reject Setctty; fall back to a pty without
		// controlling terminal, which is sufficient for interactive I/O.
		cmd = exec.CommandContext(procCtx, req.GetCommand(), req.GetArgs()...)
		if cwd := req.GetCwd(); cwd != "" {
			cmd.Dir = cwd
		}
		if env := req.GetEnv(); len(env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		ptyFile, err = startPTY(cmd, ws, false)
	}
	if err != nil {
		cancel()
		return nil, status.Error(codes.Internal, err.Error())
	}

	id := uuid.NewString()
	sess := &ptySession{
		id:     id,
		cmd:    cmd,
		f:      ptyFile,
		cancel: cancel,
		closed: make(chan struct{}),
	}

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		sess.endOnce.Do(func() {
			cancel()
			close(sess.closed)
		})
		// Keep the PTY session around briefly after the process exits so a client
		// can still attach and drain output for short-lived commands.
		time.AfterFunc(2*time.Second, func() {
			s.cleanupPTY(id, sess)
		})
	}()

	return &xrunnerv1.OpenPTYResponse{SessionId: id}, nil
}

func (s *ptyService) cleanupPTY(id string, sess *ptySession) {
	if sess == nil {
		return
	}
	sess.cleanupOnce.Do(func() {
		_ = sess.f.Close()
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
	})
}

func startPTY(cmd *exec.Cmd, ws *pty.Winsize, setCTTY bool) (*os.File, error) {
	ptyFile, ttyFile, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = ttyFile.Close() }()

	if ws != nil {
		_ = pty.Setsize(ptyFile, ws)
	}

	cmd.Stdin = ttyFile
	cmd.Stdout = ttyFile
	cmd.Stderr = ttyFile

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = setCTTY
	if setCTTY {
		cmd.SysProcAttr.Ctty = int(ttyFile.Fd())
	} else {
		cmd.SysProcAttr.Ctty = 0
	}

	if err := cmd.Start(); err != nil {
		_ = ptyFile.Close()
		return nil, err
	}
	return ptyFile, nil
}

func (s *ptyService) StreamPTY(req *xrunnerv1.PTYStreamRequest, stream xrunnerv1.RemotePTYService_StreamPTYServer) error {
	sess, err := s.get(req.GetSessionId())
	if err != nil {
		return err
	}

	const (
		maxChunk   = 64 * 1024
		flushEvery = 20 * time.Millisecond
	)
	buf := make([]byte, maxChunk)
	pending := make([]byte, 0, maxChunk)

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := stream.Send(&xrunnerv1.PTYOutputChunk{
			Timestamp: timestamppb.Now(),
			Data:      append([]byte(nil), pending...),
		})
		pending = pending[:0]
		return err
	}

	for {
		_ = sess.f.SetReadDeadline(time.Now().Add(flushEvery))
		n, rerr := sess.f.Read(buf)
		if n > 0 {
			remaining := buf[:n]
			for len(remaining) > 0 {
				room := maxChunk - len(pending)
				if room == 0 {
					if err := flush(); err != nil {
						return err
					}
					room = maxChunk
				}
				if room > len(remaining) {
					room = len(remaining)
				}
				pending = append(pending, remaining[:room]...)
				remaining = remaining[room:]
				if len(pending) >= maxChunk {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, os.ErrDeadlineExceeded) {
				if err := flush(); err != nil {
					return err
				}
				select {
				case <-stream.Context().Done():
					return stream.Context().Err()
				case <-sess.closed:
					return nil
				default:
					continue
				}
			}
			if errors.Is(rerr, io.EOF) {
				_ = flush()
				return nil
			}
			select {
			case <-sess.closed:
				_ = flush()
				return nil
			default:
				_ = flush()
				return status.Error(codes.Internal, rerr.Error())
			}
		}
		select {
		case <-stream.Context().Done():
			_ = flush()
			return stream.Context().Err()
		case <-sess.closed:
			_ = flush()
			return nil
		default:
		}
	}
}

func (s *ptyService) WritePTY(ctx context.Context, req *xrunnerv1.PTYWriteRequest) (*emptypb.Empty, error) {
	sess, err := s.get(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if len(req.GetData()) == 0 {
		return &emptypb.Empty{}, nil
	}
	if _, err := sess.f.Write(req.GetData()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *ptyService) ResizePTY(ctx context.Context, req *xrunnerv1.PTYResizeRequest) (*emptypb.Empty, error) {
	sess, err := s.get(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	ws := &pty.Winsize{Cols: 120, Rows: 30}
	if req.GetCols() > 0 {
		ws.Cols = uint16(req.GetCols())
	}
	if req.GetRows() > 0 {
		ws.Rows = uint16(req.GetRows())
	}
	if err := pty.Setsize(sess.f, ws); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *ptyService) ClosePTY(ctx context.Context, req *xrunnerv1.PTYCloseRequest) (*emptypb.Empty, error) {
	sess, err := s.get(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	sess.endOnce.Do(func() {
		sess.cancel()
		close(sess.closed)
	})
	s.cleanupPTY(req.GetSessionId(), sess)
	return &emptypb.Empty{}, nil
}

func (s *ptyService) get(id string) (*ptySession, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	return sess, nil
}
