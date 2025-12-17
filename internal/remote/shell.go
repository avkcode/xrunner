package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type shellService struct {
	xrunnerv1.UnimplementedRemoteShellServiceServer

	rootDir string

	mu        sync.Mutex
	lastAckBy map[string]map[string]uint64 // session -> client_id -> ack_offset
}

func newShellService(rootDir string) *shellService {
	return &shellService{rootDir: rootDir, lastAckBy: make(map[string]map[string]uint64)}
}

var shellNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (s *shellService) EnsureShellSession(ctx context.Context, req *xrunnerv1.EnsureShellSessionRequest) (*xrunnerv1.EnsureShellSessionResponse, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return nil, err
	}
	logPath := s.logPath(name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = touchFile(logPath, 0o644)

	exists, err := tmuxHasSession(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		shellCmd := strings.TrimSpace(req.GetShell())
		args := req.GetArgs()
		if shellCmd == "" {
			shellCmd = "sh"
		}

		// Best-effort: enable interactive command capture via shell hooks for default shells.
		// This is an implementation detail; clients see structured command events.
		if isDefaultShell(shellCmd, args) {
			hookDir := s.hookDir(name)
			if err := s.writeHookFiles(hookDir); err == nil {
				shellCmd = "sh"
				args = []string{"-lc", s.bootstrapScript(hookDir)}
			}
		}

		if err := tmuxNewSession(ctx, name, shellCmd, args, req.GetCwd(), req.GetEnv()); err != nil {
			return nil, err
		}
	}

	// Always (re)attach output piping to our log so new output is durable.
	if err := tmuxPipePane(ctx, name, logPath); err != nil {
		return nil, err
	}
	if req.GetCols() > 0 && req.GetRows() > 0 {
		_ = tmuxResize(ctx, name, int(req.GetCols()), int(req.GetRows()))
	}

	// If we just attached piping to an existing session and the log is empty,
	// seed it with a bounded capture so clients can replay some history.
	if fi, err := os.Stat(logPath); err == nil && fi.Size() == 0 {
		if cap, err := tmuxCapture(ctx, name, 2000); err == nil && len(cap) > 0 {
			_ = appendFileBounded(logPath, cap, 1<<20)
		}
	}

	size, _ := fileSize(logPath)
	return &xrunnerv1.EnsureShellSessionResponse{Name: name, LogSize: uint64(size)}, nil
}

func (s *shellService) StreamShell(req *xrunnerv1.ShellStreamRequest, stream xrunnerv1.RemoteShellService_StreamShellServer) error {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return err
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return err
	}
	if ok, err := tmuxHasSession(stream.Context(), name); err != nil {
		return err
	} else if !ok {
		return status.Error(codes.NotFound, "shell session not found")
	}

	logPath := s.logPath(name)
	if err := tmuxPipePane(stream.Context(), name, logPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	_ = touchFile(logPath, 0o644)

	maxChunk := int(req.GetMaxChunkBytes())
	if maxChunk <= 0 {
		maxChunk = 64 * 1024
	}
	if maxChunk > 256*1024 {
		maxChunk = 256 * 1024
	}
	poll := time.Duration(req.GetPollMs()) * time.Millisecond
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	if poll < 5*time.Millisecond {
		poll = 5 * time.Millisecond
	}

	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status.Error(codes.NotFound, "shell log not found")
		}
		return status.Error(codes.Internal, err.Error())
	}
	defer f.Close()

	offset := int64(req.GetOffset())
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	buf := make([]byte, maxChunk)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			err := stream.Send(&xrunnerv1.ShellOutputChunk{
				Timestamp: timestamppb.Now(),
				Offset:    uint64(offset),
				Data:      data,
			})
			offset += int64(n)
			if err != nil {
				return err
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if !req.GetFollow() {
					return nil
				}
				select {
				case <-stream.Context().Done():
					return stream.Context().Err()
				case <-time.After(poll):
					continue
				}
			}
			return status.Error(codes.Internal, rerr.Error())
		}
	}
}

func (s *shellService) WriteShell(ctx context.Context, req *xrunnerv1.ShellWriteRequest) (*emptypb.Empty, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	if len(req.GetData()) == 0 {
		return &emptypb.Empty{}, nil
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return nil, err
	}
	if ok, err := tmuxHasSession(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, status.Error(codes.NotFound, "shell session not found")
	}
	if err := tmuxSendBytes(ctx, name, req.GetData()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *shellService) ResizeShell(ctx context.Context, req *xrunnerv1.ShellResizeRequest) (*emptypb.Empty, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return nil, err
	}
	if ok, err := tmuxHasSession(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, status.Error(codes.NotFound, "shell session not found")
	}
	if req.GetCols() == 0 || req.GetRows() == 0 {
		return &emptypb.Empty{}, nil
	}
	if err := tmuxResize(ctx, name, int(req.GetCols()), int(req.GetRows())); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *shellService) TailShell(ctx context.Context, req *xrunnerv1.ShellTailRequest) (*xrunnerv1.ShellTailResponse, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return nil, err
	}
	if ok, err := tmuxHasSession(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, status.Error(codes.NotFound, "shell session not found")
	}
	logPath := s.logPath(name)
	if err := tmuxPipePane(ctx, name, logPath); err != nil {
		return nil, err
	}
	_ = touchFile(logPath, 0o644)

	lines := int(req.GetLines())
	if lines <= 0 {
		lines = 200
	}
	maxBytes := int(req.GetMaxBytes())
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	if maxBytes > 8<<20 {
		maxBytes = 8 << 20
	}
	data, err := tailFileLines(logPath, lines, maxBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, status.Error(codes.NotFound, "shell log not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	size, _ := fileSize(logPath)
	return &xrunnerv1.ShellTailResponse{
		Name:    name,
		Data:    data,
		LogSize: uint64(size),
	}, nil
}

func (s *shellService) RunShell(ctx context.Context, req *xrunnerv1.ShellRunRequest) (*xrunnerv1.ShellRunResponse, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	cmdStr := strings.TrimSpace(req.GetCommand())
	if cmdStr == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return nil, err
	}
	if ok, err := tmuxHasSession(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, status.Error(codes.NotFound, "shell session not found")
	}

	logPath := s.logPath(name)
	if err := tmuxPipePane(ctx, name, logPath); err != nil {
		return nil, err
	}
	_ = touchFile(logPath, 0o644)

	beginOffset, _ := fileSize(logPath)
	commandID := strings.TrimSpace(req.GetCommandId())
	if commandID == "" {
		commandID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	beginMarker := fmt.Sprintf("__XRUNNER_RUN_BEGIN__%s__", commandID)
	endMarker := fmt.Sprintf("__XRUNNER_RUN_END__%s__", commandID)

	wrapper := fmt.Sprintf("echo %s; %s; ec=$?; echo %s:$ec", shellEscapeForSh(beginMarker), cmdStr, shellEscapeForSh(endMarker))
	startedAt := time.Now()
	if err := tmuxSendLine(ctx, name, wrapper); err != nil {
		return nil, err
	}

	timeout := time.Duration(req.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exitCode, endOffset, err := waitForRunEnd(waitCtx, logPath, int64(beginOffset), endMarker)
	if err != nil {
		return nil, err
	}
	completedAt := time.Now()
	rec := &xrunnerv1.ShellCommandRecord{
		Name:        name,
		CommandId:   commandID,
		Command:     cmdStr,
		ExitCode:    int32(exitCode),
		BeginOffset: uint64(beginOffset),
		EndOffset:   uint64(endOffset),
		StartedAt:   timestamppb.New(startedAt),
		CompletedAt: timestamppb.New(completedAt),
		OutputBytes: uint64(endOffset) - uint64(beginOffset),
	}
	rec.ArtifactIds = append(rec.ArtifactIds, s.maybeExtractArtifacts(ctx, name, rec, logPath)...)
	_ = s.appendCommandRecord(rec)
	return &xrunnerv1.ShellRunResponse{
		Name:        name,
		CommandId:   commandID,
		ExitCode:    int32(exitCode),
		BeginOffset: uint64(beginOffset),
		EndOffset:   uint64(endOffset),
		StartedAt:   timestamppb.New(startedAt),
		CompletedAt: timestamppb.New(completedAt),
	}, nil
}

func (s *shellService) CloseShell(ctx context.Context, req *xrunnerv1.ShellCloseRequest) (*emptypb.Empty, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return nil, err
	}
	if ok, err := tmuxHasSession(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return &emptypb.Empty{}, nil
	}
	if req.GetKill() {
		_ = tmuxKillSession(ctx, name)
		_ = os.Remove(s.logPath(name))
		_ = os.Remove(s.timelinePath(name))
		_ = os.Remove(s.artifactsPath(name))
	}
	return &emptypb.Empty{}, nil
}

func (s *shellService) AttachShell(stream xrunnerv1.RemoteShellService_AttachShellServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be hello")
	}

	name, err := validateShellName(hello.GetName())
	if err != nil {
		return err
	}
	if err := s.ensureTmuxAvailable(); err != nil {
		return err
	}

	// Ensure durable session exists and log piping is active.
	_, err = s.EnsureShellSession(stream.Context(), &xrunnerv1.EnsureShellSessionRequest{Name: name})
	if err != nil {
		return err
	}

	clientID := strings.TrimSpace(hello.GetClientId())
	startOffset := hello.GetAfterOffset()
	if startOffset == 0 && hello.GetResumeFromLastAck() && clientID != "" {
		if v, ok := s.getLastAck(name, clientID); ok {
			startOffset = v
		}
	}

	explicitMaxChunk := hello.GetMaxChunkBytes() > 0
	explicitPoll := hello.GetPollMs() > 0
	maxChunk := clampInt(int(hello.GetMaxChunkBytes()), 0, 256*1024)
	poll := time.Duration(hello.GetPollMs()) * time.Millisecond
	if maxChunk == 0 {
		maxChunk = 64 * 1024
	}
	if poll <= 0 {
		poll = 25 * time.Millisecond
	}
	if poll < 5*time.Millisecond {
		poll = 5 * time.Millisecond
	}
	var maxChunkA atomic.Int32
	maxChunkA.Store(int32(maxChunk))
	var pollA atomic.Int64
	pollA.Store(int64(poll))

	compression := hello.GetCompression()
	var zenc *zstd.Encoder
	if compression == xrunnerv1.ShellClientHello_COMPRESSION_ZSTD {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		defer enc.Close()
		zenc = enc
	}

	logPath := s.logPath(name)
	_ = touchFile(logPath, 0o644)
	size, _ := fileSize(logPath)
	if startOffset > uint64(size) {
		startOffset = uint64(size)
	}

	outCh := make(chan *xrunnerv1.ShellServerMsg, 256)
	sendStop := make(chan struct{})
	var sendStopOnce sync.Once
	stopSend := func() {
		sendStopOnce.Do(func() {
			close(sendStop)
			close(outCh)
		})
	}
	go func() {
		for {
			select {
			case <-sendStop:
				return
			case msg, ok := <-outCh:
				if !ok {
					return
				}
				if msg == nil {
					continue
				}
				if err := stream.Send(msg); err != nil {
					stopSend()
					return
				}
			}
		}
	}()
	send := func(msg *xrunnerv1.ShellServerMsg) bool {
		select {
		case <-stream.Context().Done():
			stopSend()
			return false
		case <-sendStop:
			return false
		case outCh <- msg:
			return true
		}
	}

	if !send(&xrunnerv1.ShellServerMsg{Kind: &xrunnerv1.ShellServerMsg_Hello{Hello: &xrunnerv1.ShellServerHello{
		Name:        name,
		StartOffset: startOffset,
		LogSize:     uint64(size),
		Compression: compression,
	}}}) {
		stopSend()
		return stream.Context().Err()
	}

	errCh := make(chan error, 2)
	done := make(chan struct{})
	defer close(done)

	// Receive loop: input/resize/ack/close.
	var lastSentEnd atomic.Uint64
	var lastSentAt atomic.Int64
	var rttEMA atomic.Int64 // nanoseconds

	updateAdaptive := func(rtt time.Duration) {
		// EMA with alpha=0.2
		prev := time.Duration(rttEMA.Load())
		if prev <= 0 {
			rttEMA.Store(int64(rtt))
			return
		}
		next := time.Duration(float64(prev)*0.8 + float64(rtt)*0.2)
		rttEMA.Store(int64(next))
		if explicitMaxChunk && explicitPoll {
			return
		}
		// Tune for higher RTT by sending bigger chunks and polling less.
		switch {
		case next >= 300*time.Millisecond:
			if !explicitMaxChunk {
				maxChunkA.Store(256 * 1024)
			}
			if !explicitPoll {
				pollA.Store(int64(80 * time.Millisecond))
			}
		case next >= 150*time.Millisecond:
			if !explicitMaxChunk {
				maxChunkA.Store(128 * 1024)
			}
			if !explicitPoll {
				pollA.Store(int64(50 * time.Millisecond))
			}
		case next >= 80*time.Millisecond:
			if !explicitMaxChunk {
				maxChunkA.Store(96 * 1024)
			}
			if !explicitPoll {
				pollA.Store(int64(35 * time.Millisecond))
			}
		default:
			if !explicitMaxChunk {
				maxChunkA.Store(64 * 1024)
			}
			if !explicitPoll {
				pollA.Store(int64(25 * time.Millisecond))
			}
		}
	}

	go func() {
		defer func() { errCh <- io.EOF }()
		for {
			msg, rerr := stream.Recv()
			if rerr != nil {
				errCh <- rerr
				return
			}
			switch k := msg.GetKind().(type) {
			case *xrunnerv1.ShellClientMsg_Input:
				if k.Input != nil && len(k.Input.GetData()) > 0 {
					if werr := tmuxSendBytes(stream.Context(), name, k.Input.GetData()); werr != nil {
						errCh <- werr
						return
					}
				}
			case *xrunnerv1.ShellClientMsg_Resize:
				if k.Resize != nil && k.Resize.GetCols() > 0 && k.Resize.GetRows() > 0 {
					_ = tmuxResize(stream.Context(), name, int(k.Resize.GetCols()), int(k.Resize.GetRows()))
				}
			case *xrunnerv1.ShellClientMsg_Run:
				if k.Run != nil {
					cmdStr := strings.TrimSpace(k.Run.GetCommand())
					if cmdStr == "" {
						continue
					}
					timeout := time.Duration(k.Run.GetTimeoutMs()) * time.Millisecond
					if timeout <= 0 {
						timeout = 10 * time.Second
					}
					if timeout > 10*time.Minute {
						timeout = 10 * time.Minute
					}
					commandID := strings.TrimSpace(k.Run.GetCommandId())
					if commandID == "" {
						commandID = strings.ReplaceAll(uuid.NewString(), "-", "")
					}

					beginOffset, _ := fileSize(logPath)
					startedAt := time.Now()
					rec := &xrunnerv1.ShellCommandRecord{
						Name:        name,
						CommandId:   commandID,
						Command:     cmdStr,
						BeginOffset: uint64(beginOffset),
						StartedAt:   timestamppb.New(startedAt),
					}
					_ = send(&xrunnerv1.ShellServerMsg{Kind: &xrunnerv1.ShellServerMsg_Command{Command: &xrunnerv1.ShellServerCommandEvent{
						Name:   name,
						Record: rec,
						Phase:  xrunnerv1.ShellServerCommandEvent_PHASE_STARTED,
					}}})

					go func() {
						beginMarker := fmt.Sprintf("__XRUNNER_RUN_BEGIN__%s__", commandID)
						endMarker := fmt.Sprintf("__XRUNNER_RUN_END__%s__", commandID)
						wrapper := fmt.Sprintf("echo %s; %s; ec=$?; echo %s:$ec", shellEscapeForSh(beginMarker), cmdStr, shellEscapeForSh(endMarker))
						_ = tmuxSendLine(stream.Context(), name, wrapper)

						waitCtx, cancel := context.WithTimeout(stream.Context(), timeout)
						exitCode, endOffset, werr := waitForRunEnd(waitCtx, logPath, int64(beginOffset), endMarker)
						cancel()
						if werr != nil {
							return
						}
						completedAt := time.Now()
						final := &xrunnerv1.ShellCommandRecord{
							Name:        name,
							CommandId:   commandID,
							Command:     cmdStr,
							ExitCode:    int32(exitCode),
							BeginOffset: uint64(beginOffset),
							EndOffset:   uint64(endOffset),
							StartedAt:   timestamppb.New(startedAt),
							CompletedAt: timestamppb.New(completedAt),
							OutputBytes: uint64(endOffset) - uint64(beginOffset),
						}
						final.ArtifactIds = append(final.ArtifactIds, s.maybeExtractArtifacts(stream.Context(), name, final, logPath)...)
						_ = s.appendCommandRecord(final)
						_ = send(&xrunnerv1.ShellServerMsg{Kind: &xrunnerv1.ShellServerMsg_Command{Command: &xrunnerv1.ShellServerCommandEvent{
							Name:   name,
							Record: final,
							Phase:  xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED,
						}}})
					}()
				}
			case *xrunnerv1.ShellClientMsg_Ack:
				if k.Ack != nil {
					cid := strings.TrimSpace(k.Ack.GetClientId())
					if cid != "" {
						s.setLastAck(name, cid, k.Ack.GetAckOffset())
					}
					end := lastSentEnd.Load()
					if end > 0 && k.Ack.GetAckOffset() >= end {
						sent := time.Unix(0, lastSentAt.Load())
						if !sent.IsZero() {
							updateAdaptive(time.Since(sent))
						}
					}
				}
			case *xrunnerv1.ShellClientMsg_Close:
				if k.Close != nil && k.Close.GetKill() {
					_ = tmuxKillSession(stream.Context(), name)
					_ = os.Remove(logPath)
				}
				errCh <- io.EOF
				return
			default:
				// ignore
			}
		}
	}()

	// Send loop: tail the durable log from startOffset.
	go func() {
		f, oerr := os.Open(logPath)
		if oerr != nil {
			errCh <- status.Error(codes.Internal, oerr.Error())
			return
		}
		defer f.Close()
		if _, oerr := f.Seek(int64(startOffset), io.SeekStart); oerr != nil {
			errCh <- status.Error(codes.Internal, oerr.Error())
			return
		}
		tmp := make([]byte, 256*1024)
		offset := int64(startOffset)
		var pending []byte
		var pendingStart int64
		lastFlush := time.Now()
		cmdByID := make(map[string]*xrunnerv1.ShellCommandRecord)

		flush := func() error {
			if len(pending) == 0 {
				return nil
			}
			// Avoid splitting an OSC marker across chunks.
			sendLen := flushablePrefixLen(pending)
			if sendLen == 0 {
				return nil
			}

			uncompressed := pending[:sendLen]
			clean, markers := stripXRMarkers(uncompressed, pendingStart)
			for _, m := range markers {
				switch m.kind {
				case "cmd_start":
					rec := &xrunnerv1.ShellCommandRecord{
						Name:        name,
						CommandId:   m.commandID,
						Command:     m.command,
						BeginOffset: uint64(m.endOffset),
						StartedAt:   timestamppb.New(time.Unix(0, m.tsNS)),
					}
					cmdByID[m.commandID] = rec
					_ = send(&xrunnerv1.ShellServerMsg{Kind: &xrunnerv1.ShellServerMsg_Command{Command: &xrunnerv1.ShellServerCommandEvent{
						Name:   name,
						Record: rec,
						Phase:  xrunnerv1.ShellServerCommandEvent_PHASE_STARTED,
					}}})
				case "cmd_end":
					rec := cmdByID[m.commandID]
					if rec == nil {
						rec = &xrunnerv1.ShellCommandRecord{Name: name, CommandId: m.commandID}
					}
					rec.ExitCode = int32(m.exitCode)
					rec.CompletedAt = timestamppb.New(time.Unix(0, m.tsNS))
					rec.EndOffset = uint64(m.startOffset)
					if rec.BeginOffset > 0 && rec.EndOffset > rec.BeginOffset {
						rec.OutputBytes = rec.EndOffset - rec.BeginOffset
					}
					// Use begin_offset/end_offset derived from output to compute artifacts.
					rec.ArtifactIds = append(rec.ArtifactIds, s.maybeExtractArtifacts(stream.Context(), name, rec, logPath)...)
					_ = s.appendCommandRecord(rec)
					_ = send(&xrunnerv1.ShellServerMsg{Kind: &xrunnerv1.ShellServerMsg_Command{Command: &xrunnerv1.ShellServerCommandEvent{
						Name:   name,
						Record: rec,
						Phase:  xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED,
					}}})
					delete(cmdByID, m.commandID)
				}
			}

			data := clean
			if zenc != nil && len(clean) >= 256 {
				data = zenc.EncodeAll(clean, make([]byte, 0, len(clean)))
			}
			lastSentEnd.Store(uint64(pendingStart) + uint64(len(uncompressed)))
			lastSentAt.Store(time.Now().UnixNano())
			ok := send(&xrunnerv1.ShellServerMsg{Kind: &xrunnerv1.ShellServerMsg_Chunk{Chunk: &xrunnerv1.ShellServerChunk{
				Name:            name,
				Offset:          uint64(pendingStart),
				Data:            data,
				UncompressedLen: uint32(len(uncompressed)),
			}}})
			// Advance pending window.
			pending = pending[sendLen:]
			pendingStart += int64(sendLen)
			lastFlush = time.Now()
			if !ok {
				return io.EOF
			}
			if clientID != "" {
				s.setLastAck(name, clientID, lastSentEnd.Load())
			}
			return nil
		}

		for {
			n, rerr := f.Read(tmp)
			if n > 0 {
				if len(pending) == 0 {
					pendingStart = offset
				}
				pending = append(pending, tmp[:n]...)
				offset += int64(n)
				curMaxChunk := int(maxChunkA.Load())
				// Burst buffering: flush when full or after a short interval.
				if len(pending) >= curMaxChunk || time.Since(lastFlush) >= 15*time.Millisecond {
					if err := flush(); err != nil {
						errCh <- err
						return
					}
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					if err := flush(); err != nil {
						errCh <- err
						return
					}
					select {
					case <-stream.Context().Done():
						errCh <- stream.Context().Err()
						return
					case <-done:
						errCh <- io.EOF
						return
					case <-time.After(time.Duration(pollA.Load())):
						continue
					}
				}
				errCh <- status.Error(codes.Internal, rerr.Error())
				return
			}
		}
	}()

	select {
	case <-stream.Context().Done():
		stopSend()
		return stream.Context().Err()
	case err := <-errCh:
		stopSend()
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

func (s *shellService) ensureTmuxAvailable() error {
	_, err := exec.LookPath("tmux")
	if err == nil {
		return nil
	}
	return status.Error(codes.FailedPrecondition, "tmux is required for durable shell sessions")
}

func (s *shellService) ListShellCommands(ctx context.Context, req *xrunnerv1.ListShellCommandsRequest) (*xrunnerv1.ListShellCommandsResponse, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	cmds, err := s.readLastCommandRecords(name, limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &xrunnerv1.ListShellCommandsResponse{Commands: cmds}, nil
}

func (s *shellService) SearchShell(ctx context.Context, req *xrunnerv1.ShellSearchRequest) (*xrunnerv1.ShellSearchResponse, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	q := req.GetQuery()
	if len(q) == 0 {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	logPath := s.logPath(name)
	size, _ := fileSize(logPath)

	start := req.GetStartOffset()
	end := req.GetEndOffset()
	if end == 0 || end > uint64(size) {
		end = uint64(size)
	}
	if start > end {
		start = end
	}

	// Narrow by command_id / time range if requested (based on recorded RunShell timeline).
	var allowed []*xrunnerv1.ShellCommandRecord
	if cid := strings.TrimSpace(req.GetCommandId()); cid != "" || req.GetStartTime() != nil || req.GetEndTime() != nil {
		all, err := s.readLastCommandRecords(name, 5000)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		for _, r := range all {
			if r == nil {
				continue
			}
			if cid != "" && r.GetCommandId() != cid {
				continue
			}
			if req.GetStartTime() != nil && r.GetStartedAt() != nil && r.GetStartedAt().AsTime().Before(req.GetStartTime().AsTime()) {
				continue
			}
			if req.GetEndTime() != nil && r.GetStartedAt() != nil && r.GetStartedAt().AsTime().After(req.GetEndTime().AsTime()) {
				continue
			}
			allowed = append(allowed, r)
		}
		if cid != "" && len(allowed) == 0 {
			return &xrunnerv1.ShellSearchResponse{LogSize: uint64(size)}, nil
		}
	}

	maxMatches := int(req.GetMaxMatches())
	if maxMatches <= 0 {
		maxMatches = 50
	}
	if maxMatches > 500 {
		maxMatches = 500
	}
	contextBytes := int(req.GetContextBytes())
	if contextBytes <= 0 {
		contextBytes = 80
	}
	if contextBytes > 4096 {
		contextBytes = 4096
	}

	matches := make([]*xrunnerv1.ShellSearchMatch, 0, min(maxMatches, 50))
	searchRange := func(cmdID string, a, b uint64) error {
		found, err := searchLogRange(ctx, logPath, q, a, b, maxMatches-len(matches), contextBytes, func(off uint64, snippet []byte) {
			matches = append(matches, &xrunnerv1.ShellSearchMatch{CommandId: cmdID, Offset: off, Snippet: snippet})
		})
		_ = found
		return err
	}

	if len(allowed) > 0 {
		for _, r := range allowed {
			a := maxU64(start, r.GetBeginOffset())
			b := minU64(end, r.GetEndOffset())
			if b <= a {
				continue
			}
			if err := searchRange(r.GetCommandId(), a, b); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			if len(matches) >= maxMatches {
				break
			}
		}
	} else {
		if err := searchRange("", start, end); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &xrunnerv1.ShellSearchResponse{Matches: matches, LogSize: uint64(size)}, nil
}

func (s *shellService) ListShellArtifacts(ctx context.Context, req *xrunnerv1.ListShellArtifactsRequest) (*xrunnerv1.ListShellArtifactsResponse, error) {
	name, err := validateShellName(req.GetName())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	arts, err := s.readLastArtifacts(name, strings.TrimSpace(req.GetCommandId()), limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &xrunnerv1.ListShellArtifactsResponse{Artifacts: arts}, nil
}

func (s *shellService) getLastAck(session string, clientID string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.lastAckBy[session]
	if m == nil {
		return 0, false
	}
	v, ok := m[clientID]
	return v, ok
}

func (s *shellService) setLastAck(session string, clientID string, offset uint64) {
	if clientID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.lastAckBy[session]
	if m == nil {
		m = make(map[string]uint64)
		s.lastAckBy[session] = m
	}
	if offset > m[clientID] {
		m[clientID] = offset
	}
}

func validateShellName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", status.Error(codes.InvalidArgument, "name is required")
	}
	if !shellNameRe.MatchString(name) {
		return "", status.Error(codes.InvalidArgument, "invalid session name (allowed: [a-zA-Z0-9._-], max 64 chars)")
	}
	return name, nil
}

func (s *shellService) logPath(name string) string {
	return filepath.Join(s.rootDir, "shell", name+".log")
}

func (s *shellService) hookDir(name string) string {
	return filepath.Join(s.rootDir, "shell", name+".hooks")
}

func (s *shellService) timelinePath(name string) string {
	return filepath.Join(s.rootDir, "shell", name+".timeline.bin")
}

func (s *shellService) artifactsPath(name string) string {
	return filepath.Join(s.rootDir, "shell", name+".artifacts.bin")
}

func isDefaultShell(shellCmd string, args []string) bool {
	shellCmd = strings.TrimSpace(shellCmd)
	if shellCmd == "" {
		return true
	}
	if shellCmd == "sh" && len(args) == 0 {
		return true
	}
	if shellCmd == "bash" && len(args) == 0 {
		return true
	}
	if shellCmd == "zsh" && len(args) == 0 {
		return true
	}
	return false
}

func (s *shellService) writeHookFiles(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zshrc := filepath.Join(dir, ".zshrc")
	bashrc := filepath.Join(dir, "bashrc")
	if err := os.WriteFile(zshrc, []byte(zshHookScript), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(bashrc, []byte(bashHookScript), 0o644); err != nil {
		return err
	}
	return nil
}

func (s *shellService) bootstrapScript(hookDir string) string {
	// Use zsh if available (best hooks), else bash, else sh fallback.
	// Note: hookDir is created by writeHookFiles; quote it for shell safety.
	hd := shellQuoteSingle(hookDir)
	return strings.Join([]string{
		"if command -v zsh >/dev/null 2>&1; then",
		"  export ZDOTDIR=" + hd + "; exec zsh -i;",
		"elif command -v bash >/dev/null 2>&1; then",
		"  exec bash --rcfile " + shellQuoteSingle(filepath.Join(hookDir, "bashrc")) + " -i;",
		"else",
		"  exec sh;",
		"fi",
	}, " ")
}

const zshHookScript = `
__xr__now_ns() { date +%s%N; }
__xr__b64() { printf "%s" "$1" | base64 | tr -d '\n'; }
__xr__emit() { printf '\033]777;XR;%s\007' "$1"; }

preexec() {
  local cmd="$1"
  local id="$(date +%s%N)${RANDOM}"
  __XR_CMD_ID="$id"
  __xr__emit "cmd_start;${id};$(__xr__now_ns);$(__xr__b64 "$cmd")"
}

precmd() {
  local ec="$?"
  if [[ -n "${__XR_CMD_ID:-}" ]]; then
    __xr__emit "cmd_end;${__XR_CMD_ID};$(__xr__now_ns);${ec}"
    unset __XR_CMD_ID
  fi
}
`

const bashHookScript = `
__xr__now_ns() { date +%s%N; }
__xr__b64() { printf "%s" "$1" | base64 | tr -d '\n'; }
__xr__emit() { printf '\033]777;XR;%s\007' "$1"; }

__xr__preexec() {
  # DEBUG trap fires for many internal commands; guard against re-entrancy and hook noise.
  [[ -n "${__XR_IN_HOOK:-}" ]] && return 0
  __XR_IN_HOOK=1
  local cmd="${BASH_COMMAND}"
  case "${cmd}" in
    __xr__*|trap*|history*|PROMPT_COMMAND*|printf*|builtin* ) __XR_IN_HOOK=; return 0 ;;
  esac
  local id="$(date +%s%N)${RANDOM}"
  __XR_CMD_ID="${id}"
  __xr__emit "cmd_start;${id};$(__xr__now_ns);$(__xr__b64 "${cmd}")"
  __XR_IN_HOOK=
}

__xr__precmd() {
  local ec="$?"
  if [[ -n "${__XR_CMD_ID:-}" ]]; then
    __xr__emit "cmd_end;${__XR_CMD_ID};$(__xr__now_ns);${ec}"
    unset __XR_CMD_ID
  fi
}

trap '__xr__preexec' DEBUG
PROMPT_COMMAND="__xr__precmd; ${PROMPT_COMMAND}"
`

func tmuxHasSession(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "tmux", "has-session", "-t", name)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// tmux returns exit 1 when the session doesn't exist.
		return false, nil
	}
	return false, status.Error(codes.Internal, err.Error())
}

func (s *shellService) appendCommandRecord(rec *xrunnerv1.ShellCommandRecord) error {
	if rec == nil || rec.GetName() == "" {
		return nil
	}
	path := s.timelinePath(rec.GetName())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := proto.Marshal(rec)
	if err != nil {
		return err
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	if _, err := f.Write(hdr[:n]); err != nil {
		return err
	}
	_, err = f.Write(b)
	return err
}

func (s *shellService) appendArtifact(art *xrunnerv1.ShellArtifact) error {
	if art == nil || art.GetName() == "" || art.GetId() == "" {
		return nil
	}
	path := s.artifactsPath(art.GetName())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := proto.Marshal(art)
	if err != nil {
		return err
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	if _, err := f.Write(hdr[:n]); err != nil {
		return err
	}
	_, err = f.Write(b)
	return err
}

func (s *shellService) readLastArtifacts(name string, commandID string, limit int) ([]*xrunnerv1.ShellArtifact, error) {
	path := s.artifactsPath(name)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	var all []*xrunnerv1.ShellArtifact
	for {
		msg, err := readDelimited(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		art := &xrunnerv1.ShellArtifact{}
		if err := proto.Unmarshal(msg, art); err != nil {
			continue
		}
		if strings.TrimSpace(commandID) != "" && art.GetCommandId() != commandID {
			continue
		}
		all = append(all, art)
	}
	if limit <= 0 {
		return all, nil
	}
	if len(all) <= limit {
		return all, nil
	}
	return all[len(all)-limit:], nil
}

func (s *shellService) maybeExtractArtifacts(ctx context.Context, name string, rec *xrunnerv1.ShellCommandRecord, logPath string) []string {
	if rec == nil || strings.TrimSpace(rec.GetCommand()) == "" {
		return nil
	}
	if rec.GetEndOffset() <= rec.GetBeginOffset() {
		return nil
	}
	// Keep bounded.
	maxRead := uint64(2 * 1024 * 1024)
	size := rec.GetEndOffset() - rec.GetBeginOffset()
	start := rec.GetBeginOffset()
	if size > maxRead {
		start = rec.GetEndOffset() - maxRead
	}
	snippet, err := readLogRange(ctx, logPath, start, rec.GetEndOffset())
	if err != nil {
		return nil
	}

	title, payload, ok := extractGoTestFailureArtifact(rec.GetCommand(), string(snippet))
	if !ok {
		return nil
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	art := &xrunnerv1.ShellArtifact{
		Id:        id,
		Name:      name,
		CommandId: rec.GetCommandId(),
		Type:      "go_test_failure",
		Title:     title,
		DataJson:  payload,
		CreatedAt: timestamppb.Now(),
	}
	if err := s.appendArtifact(art); err != nil {
		return nil
	}
	return []string{id}
}

func readLogRange(ctx context.Context, path string, start uint64, end uint64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(start), io.SeekStart); err != nil {
		return nil, err
	}
	n := int64(end - start)
	if n < 0 {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return b, nil
}

func (s *shellService) readLastCommandRecords(name string, limit int) ([]*xrunnerv1.ShellCommandRecord, error) {
	path := s.timelinePath(name)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	var all []*xrunnerv1.ShellCommandRecord
	for {
		msg, err := readDelimited(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		rec := &xrunnerv1.ShellCommandRecord{}
		if err := proto.Unmarshal(msg, rec); err != nil {
			continue
		}
		all = append(all, rec)
	}
	if limit <= 0 || len(all) <= limit {
		return all, nil
	}
	return all[len(all)-limit:], nil
}

func readDelimited(r *bufio.Reader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return b, err
}

func searchLogRange(ctx context.Context, path string, query []byte, start uint64, end uint64, maxMatches int, contextBytes int, onMatch func(off uint64, snippet []byte)) (int, error) {
	if maxMatches <= 0 {
		return 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(start), io.SeekStart); err != nil {
		return 0, err
	}

	const chunk = 256 * 1024
	overlap := len(query) - 1 + contextBytes
	if overlap < 0 {
		overlap = 0
	}
	buf := make([]byte, chunk)
	var carry []byte
	var base uint64 = start
	found := 0

	for base < end {
		select {
		case <-ctx.Done():
			return found, ctx.Err()
		default:
		}
		toRead := int(minU64(uint64(len(buf)), end-base))
		n, err := io.ReadFull(f, buf[:toRead])
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return found, err
		}
		window := append(append([]byte(nil), carry...), buf[:n]...)
		windowBase := base - uint64(len(carry))
		idx := 0
		for {
			j := bytes.Index(window[idx:], query)
			if j < 0 {
				break
			}
			pos := idx + j
			off := windowBase + uint64(pos)
			if off < start || off >= end {
				idx = pos + 1
				continue
			}
			s0 := maxInt(0, pos-contextBytes)
			s1 := minInt(len(window), pos+len(query)+contextBytes)
			snip := append([]byte(nil), window[s0:s1]...)
			onMatch(off, snip)
			found++
			if found >= maxMatches {
				return found, nil
			}
			idx = pos + max(1, len(query))
		}
		if n == 0 {
			break
		}
		if overlap > 0 && len(window) > overlap {
			carry = append([]byte(nil), window[len(window)-overlap:]...)
		} else {
			carry = window
		}
		base += uint64(n)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
	}
	return found, nil
}

func clampInt(v int, lo int, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxU64(a uint64, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minU64(a uint64, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

type xrMarker struct {
	kind      string
	commandID string
	command   string
	tsNS      int64
	exitCode  int

	startOffset int64 // absolute log offset where marker starts
	endOffset   int64 // absolute log offset where marker ends
}

var xrOscPrefix = []byte{0x1b, ']', '7', '7', '7', ';', 'X', 'R', ';'}

func flushablePrefixLen(pending []byte) int {
	// If the buffer ends with an incomplete XR OSC frame, keep it for the next read.
	// To avoid deadlocks on malformed output, only withhold if the tail is small.
	last := bytes.LastIndex(pending, xrOscPrefix)
	if last < 0 {
		return len(pending)
	}
	if bytes.IndexByte(pending[last:], 0x07) >= 0 {
		return len(pending)
	}
	// Incomplete frame: keep the tail (likely < 1KB). If it's huge, flush anyway.
	if len(pending)-last > 4096 {
		return len(pending)
	}
	return last
}

func stripXRMarkers(b []byte, baseOffset int64) ([]byte, []xrMarker) {
	if len(b) == 0 {
		return b, nil
	}
	var out []byte
	var markers []xrMarker

	i := 0
	for i < len(b) {
		j := bytes.Index(b[i:], xrOscPrefix)
		if j < 0 {
			out = append(out, b[i:]...)
			break
		}
		j += i
		// Copy data before marker.
		out = append(out, b[i:j]...)
		// Find BEL terminator.
		k := bytes.IndexByte(b[j:], 0x07)
		if k < 0 {
			// Incomplete marker; keep as normal bytes (will be handled by flushablePrefixLen).
			out = append(out, b[j:]...)
			break
		}
		k += j
		payload := b[j+len(xrOscPrefix) : k]
		if m, ok := parseXRMarkerPayload(payload, baseOffset+int64(j), baseOffset+int64(k)+1); ok {
			markers = append(markers, m)
		}
		// Skip the entire OSC frame.
		i = k + 1
	}
	return out, markers
}

func parseXRMarkerPayload(payload []byte, startOff int64, endOff int64) (xrMarker, bool) {
	// Expected:
	// cmd_start;<id>;<ts_ns>;<b64>
	// cmd_end;<id>;<ts_ns>;<exit>
	parts := bytes.Split(payload, []byte(";"))
	if len(parts) < 4 {
		return xrMarker{}, false
	}
	kind := string(parts[0])
	id := string(parts[1])
	if kind != "cmd_start" && kind != "cmd_end" {
		return xrMarker{}, false
	}
	ts, err := strconv.ParseInt(string(parts[2]), 10, 64)
	if err != nil {
		ts = time.Now().UnixNano()
	}
	m := xrMarker{
		kind:        kind,
		commandID:   id,
		tsNS:        ts,
		startOffset: startOff,
		endOffset:   endOff,
	}
	if kind == "cmd_start" {
		dec, err := base64.StdEncoding.DecodeString(string(parts[3]))
		if err == nil {
			m.command = string(dec)
		} else {
			m.command = string(parts[3])
		}
		return m, true
	}
	exit, err := strconv.Atoi(string(parts[3]))
	if err != nil {
		exit = 0
	}
	m.exitCode = exit
	return m, true
}

func tmuxNewSession(ctx context.Context, name string, shellCmd string, args []string, cwd string, env map[string]string) error {
	tmuxArgs := []string{"new-session", "-d", "-s", name}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		tmuxArgs = append(tmuxArgs, "-c", cwd)
	}
	if len(env) > 0 {
		for k, v := range env {
			if strings.TrimSpace(k) == "" {
				continue
			}
			tmuxArgs = append(tmuxArgs, "-e", fmt.Sprintf("%s=%s", k, v))
		}
	}
	tmuxArgs = append(tmuxArgs, shellCmd)
	tmuxArgs = append(tmuxArgs, args...)
	out, err := exec.CommandContext(ctx, "tmux", tmuxArgs...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return status.Error(codes.Internal, msg)
	}
	return nil
}

func tmuxPipePane(ctx context.Context, name string, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	_ = touchFile(logPath, 0o644)
	// tmux executes the command via a shell; quote path defensively.
	pipeCmd := fmt.Sprintf("cat >> %s", shellQuoteSingle(logPath))
	out, err := exec.CommandContext(ctx, "tmux", "pipe-pane", "-t", paneTarget(name), pipeCmd).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return status.Error(codes.Internal, msg)
	}
	return nil
}

func tmuxResize(ctx context.Context, name string, cols int, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	out, err := exec.CommandContext(ctx, "tmux", "resize-pane", "-t", paneTarget(name), "-x", fmt.Sprintf("%d", cols), "-y", fmt.Sprintf("%d", rows)).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return status.Error(codes.Internal, msg)
	}
	return nil
}

func tmuxCapture(ctx context.Context, name string, lines int) ([]byte, error) {
	if lines <= 0 {
		lines = 2000
	}
	out, err := exec.CommandContext(ctx, "tmux", "capture-pane", "-pJ", "-t", paneTarget(name), "-S", fmt.Sprintf("-%d", lines)).Output()
	if err != nil {
		return nil, err
	}
	// Normalize to \n to avoid platform variance.
	out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	return out, nil
}

func tmuxKillSession(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "tmux", "kill-session", "-t", name).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return status.Error(codes.Internal, msg)
	}
	return nil
}

func tmuxSendLine(ctx context.Context, name string, line string) error {
	if strings.ContainsAny(line, "\r\n") {
		line = strings.ReplaceAll(line, "\r", "")
		line = strings.ReplaceAll(line, "\n", " ")
	}
	return tmuxSendBytes(ctx, name, append([]byte(line), '\n'))
}

func tmuxSendBytes(ctx context.Context, name string, data []byte) error {
	if !utf8.Valid(data) {
		return status.Error(codes.InvalidArgument, "input must be valid UTF-8")
	}
	args := []string{"send-keys", "-t", paneTarget(name)}
	parts := splitTmuxKeys(data)
	args = append(args, parts...)
	out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return status.Error(codes.Internal, msg)
	}
	return nil
}

func paneTarget(name string) string {
	// Always use the first window/pane; durable sessions are 1-pane shells for now.
	return name + ":0.0"
}

func splitTmuxKeys(data []byte) []string {
	out := make([]string, 0, 8)
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out = append(out, "-l", cur.String())
		cur.Reset()
	}
	for _, r := range string(data) {
		switch r {
		case '\r':
			continue
		case '\n':
			flush()
			out = append(out, "C-m")
		default:
			// tmux send-keys uses "key names" for control chars; we map a few
			// common ones for interactive UX.
			if r == 3 { // ETX
				flush()
				out = append(out, "C-c")
				continue
			}
			if r == 4 { // EOT
				flush()
				out = append(out, "C-d")
				continue
			}
			if r == 26 { // SUB
				flush()
				out = append(out, "C-z")
				continue
			}
			if r == 12 { // FF
				flush()
				out = append(out, "C-l")
				continue
			}
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func waitForRunEnd(ctx context.Context, logPath string, startOffset int64, endMarker string) (exitCode int, endOffset int64, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		return 0, 0, status.Error(codes.Internal, err.Error())
	}
	defer f.Close()
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return 0, 0, status.Error(codes.Internal, err.Error())
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	var buf bytes.Buffer
	poll := 25 * time.Millisecond

	for {
		for {
			line, rerr := reader.ReadBytes('\n')
			if len(line) > 0 {
				buf.Write(line)
				if code, ok := parseEndMarker(buf.Bytes(), endMarker); ok {
					pos, _ := f.Seek(0, io.SeekCurrent)
					return code, pos, nil
				}
				if buf.Len() > 2<<20 {
					// Keep the scan bounded; if marker is missing, surface a timeout.
					buf.Reset()
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					break
				}
				return 0, 0, status.Error(codes.Internal, rerr.Error())
			}
		}
		select {
		case <-ctx.Done():
			return 0, 0, status.Error(codes.DeadlineExceeded, "command timed out waiting for completion marker")
		case <-time.After(poll):
			// Refresh reader on the same fd; the next ReadBytes will see appended bytes.
			continue
		}
	}
}

func parseEndMarker(b []byte, endMarker string) (int, bool) {
	// Expected: "__XRUNNER_RUN_END__<id>__:<exit>"
	idx := bytes.LastIndex(b, []byte(endMarker+":"))
	if idx < 0 {
		return 0, false
	}
	rest := b[idx+len(endMarker)+1:]
	// Only accept digits immediately following.
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 {
		return 0, false
	}
	var code int
	_, _ = fmt.Sscanf(string(rest[:n]), "%d", &code)
	return code, true
}

func shellQuoteSingle(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellEscapeForSh(s string) string {
	// Best-effort: safe for echo arg in sh.
	return strings.ReplaceAll(s, "'", "")
}

func touchFile(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE, mode)
	if err != nil {
		return err
	}
	return f.Close()
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func appendFileBounded(path string, data []byte, maxBytes int) error {
	if maxBytes <= 0 {
		return nil
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func tailFileLines(path string, lines int, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	start := size - int64(maxBytes)
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	if err != nil {
		return nil, err
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if lines <= 0 {
		return data, nil
	}
	parts := bytes.Split(data, []byte("\n"))
	if len(parts) <= lines+1 {
		return data, nil
	}
	// Drop leading partial line if we started mid-file.
	startIdx := len(parts) - (lines + 1)
	out := bytes.Join(parts[startIdx:], []byte("\n"))
	// Ensure trailing newline for terminal-friendly output.
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}
