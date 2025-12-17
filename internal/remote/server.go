package remote

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/client"
	"github.com/antonkrylov/xrunner/internal/remote/sessionstore"
)

type Config struct {
	ListenAddr    string
	UpstreamAddr  string
	UpstreamTLS   bool
	WorkspaceRoot string
	UnsafeRootFS  bool
	SessionsDir   string

	Version   string
	Commit    string
	BuildTime string
	Logger    *slog.Logger
}

type Server struct {
	cfg Config

	grpcServer *grpc.Server
	listener   net.Listener

	upstreamConn *grpc.ClientConn
}

func New(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:7337"
	}
	if cfg.UpstreamAddr == "" {
		cfg.UpstreamAddr = "127.0.0.1:50051"
	}
	if cfg.SessionsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			cfg.SessionsDir = ".xrunner/sessions"
		} else {
			cfg.SessionsDir = filepath.Join(home, ".xrunner", "sessions")
		}
	}
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = "/"
		cfg.UnsafeRootFS = true
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	return &Server{cfg: cfg}, nil
}

func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.listener = lis

	mode := client.DialInsecure
	if s.cfg.UpstreamTLS {
		mode = client.DialTLS
	}
	dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout())
	upClient, upConn, err := client.DialJobService(dialCtx, s.cfg.UpstreamAddr, mode)
	cancel()
	if err != nil {
		_ = lis.Close()
		return fmt.Errorf("dial upstream JobService: %w", err)
	}
	s.upstreamConn = upConn

	s.grpcServer = grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    60 * time.Second,
			Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             20 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	xrunnerv1.RegisterRemoteControlServiceServer(s.grpcServer, &controlService{cfg: s.cfg})
	xrunnerv1.RegisterRemoteFileServiceServer(s.grpcServer, &fileService{
		root:         s.cfg.WorkspaceRoot,
		unsafeRootFS: s.cfg.UnsafeRootFS,
	})
	ptySvc := newPTYService()
	xrunnerv1.RegisterRemotePTYServiceServer(s.grpcServer, ptySvc)
	xrunnerv1.RegisterRemoteShellServiceServer(s.grpcServer, newShellService(s.cfg.SessionsDir))
	xrunnerv1.RegisterJobServiceServer(s.grpcServer, &jobProxy{upstream: upClient})
	ss := newSessionService(sessionstore.New(s.cfg.SessionsDir), s.cfg.WorkspaceRoot, s.cfg.Logger)
	xrunnerv1.RegisterSessionServiceServer(s.grpcServer, ss)

	reflection.Register(s.grpcServer)

	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	go func() {
		_ = s.grpcServer.Serve(lis)
	}()
	return nil
}

func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) Stop() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.upstreamConn != nil {
		_ = s.upstreamConn.Close()
	}
}

func defaultDialTimeout() (d time.Duration) {
	return 10 * time.Second
}
