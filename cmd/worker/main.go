package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/worker"
)

func main() {
	var (
		listenAddr          = flag.String("listen", ":50052", "worker gRPC listen address")
		nsjailConfig        = flag.String("nsjail-config", "configs/default.nsjail.cfg", "path to nsjail config")
		workspaceRoot       = flag.String("workspace", "/tmp/xrunner/workspaces", "workspace root for job execution")
		allowUnsafeFallback = flag.Bool("allow-unsafe-fallback", true, "allow running jobs without nsjail if binary missing")
		logJSON             = flag.Bool("log-json", false, "emit logs as JSON")
		defaultSandboxType  = flag.String("default-sandbox-type", "nsjail", "default sandbox: none|nsjail|nsjail-docker|docker")
		defaultSandboxBin   = flag.String("default-sandbox-bin", "", "override sandbox binary (defaults to nsjail in PATH)")
		defaultSandboxWork  = flag.String("default-sandbox-workdir", "", "working directory inside sandbox (defaults to workspace)")
		defaultSandboxLog   = flag.String("default-sandbox-log", "", "path to sandbox log output")
	)
	var defaultSandboxBinds stringSliceFlag
	var defaultSandboxExtra stringSliceFlag
	flag.Var(&defaultSandboxBinds, "default-sandbox-bind", "nsjail bind mount spec src:dst[:mode]; repeatable")
	flag.Var(&defaultSandboxExtra, "default-sandbox-extra-arg", "extra flag passed to sandbox binary; repeatable")
	flag.Parse()

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	logger := slog.New(handler)

	if err := os.MkdirAll(*workspaceRoot, 0o755); err != nil {
		logger.Error("workspace init", "err", err)
		os.Exit(1)
	}

	defaultSandbox, err := buildDefaultSandbox(*defaultSandboxType, *nsjailConfig, *defaultSandboxBin, *defaultSandboxWork, *defaultSandboxLog, defaultSandboxBinds, defaultSandboxExtra)
	if err != nil {
		logger.Error("configure sandbox", "err", err)
		os.Exit(1)
	}
	if defaultSandbox != nil && defaultSandbox.GetWorkdir() == "" {
		defaultSandbox.Workdir = *workspaceRoot
	}

	runner := &worker.Runner{
		WorkspaceRoot:       *workspaceRoot,
		AllowUnsafeFallback: *allowUnsafeFallback,
		Logger:              logger,
		DefaultSandbox:      defaultSandbox,
	}

	srv := worker.NewServer(runner, logger)
	grpcServer := grpc.NewServer()
	xrunnerv1.RegisterWorkerServiceServer(grpcServer, srv)
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		logger.Info("shutting down worker")
		grpcServer.GracefulStop()
	}()

	logger.Info("worker ready", "addr", *listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func buildDefaultSandbox(kind, configPath, bin, workdir, logPath string, binds, extra []string) (*xrunnerv1.SandboxSpec, error) {
	sType, err := parseSandboxType(kind)
	if err != nil {
		return nil, err
	}
	if sType == xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED {
		return nil, nil
	}
	spec := &xrunnerv1.SandboxSpec{
		Type:         sType,
		NsjailConfig: configPath,
		SandboxBin:   bin,
		Workdir:      workdir,
		LogPath:      logPath,
		Binds:        append([]string(nil), binds...),
		ExtraArgs:    append([]string(nil), extra...),
	}
	return spec, nil
}

func parseSandboxType(val string) (xrunnerv1.SandboxType, error) {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "", "none":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, nil
	case "nsjail":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_NSJAIL, nil
	case "nsjail-docker":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_NSJAIL_DOCKER, nil
	case "docker":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_DOCKER, nil
	default:
		return xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, errors.New("unsupported sandbox type: " + val)
	}
}
