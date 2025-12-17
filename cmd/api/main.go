package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/control/jobsvc"
	"github.com/antonkrylov/xrunner/internal/control/store"
)

func main() {
	var (
		listenAddr      = flag.String("listen", ":50051", "API gRPC listen address")
		workerAddr      = flag.String("worker", "localhost:50052", "worker gRPC target")
		logJSON         = flag.Bool("log-json", false, "emit logs as JSON")
		enableJetStream = flag.Bool("enable-jetstream", false, "persist jobs/logs to NATS JetStream")
		natsURL         = flag.String("nats-url", "", "NATS connection URL (XRUNNER_NATS_URL)")
		natsUser        = flag.String("nats-user", "", "NATS username (XRUNNER_NATS_USER)")
		natsPass        = flag.String("nats-pass", "", "NATS password (XRUNNER_NATS_PASS)")
		natsEventsPref  = flag.String("nats-events-prefix", "events", "NATS subject prefix for events")
		natsJobsStream  = flag.String("nats-jobs-stream", "xrunner_jobs", "JetStream stream for job snapshots")
		natsLogsStream  = flag.String("nats-logs-stream", "xrunner_logs", "JetStream stream for log chunks")
	)

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}

	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(0)
	}

	flag.Parse()

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	logger := slog.New(handler)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	applyEnvFallback(natsURL, "XRUNNER_NATS_URL")
	applyEnvFallback(natsUser, "XRUNNER_NATS_USER")
	applyEnvFallback(natsPass, "XRUNNER_NATS_PASS")

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	workerConn, err := grpc.DialContext(dialCtx, *workerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		logger.Error("worker dial", "err", err)
		os.Exit(1)
	}
	defer workerConn.Close()

	storeOpts := &store.Options{Logger: logger}
	if *enableJetStream {
		if *natsURL == "" {
			logger.Error("enable-jetstream requires --nats-url or XRUNNER_NATS_URL")
			os.Exit(1)
		}
		storeOpts.JetStream = &store.JetStreamOptions{
			URL:          *natsURL,
			User:         *natsUser,
			Password:     *natsPass,
			EventsPrefix: *natsEventsPref,
			JobsStream:   *natsJobsStream,
			LogsStream:   *natsLogsStream,
		}
	}
	st, err := store.New(ctx, storeOpts)
	if err != nil {
		logger.Error("store init", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	jobService := jobsvc.New(st, xrunnerv1.NewWorkerServiceClient(workerConn), logger)
	grpcServer := grpc.NewServer()
	xrunnerv1.RegisterJobServiceServer(grpcServer, jobService)
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down api")
		jobService.Close()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			grpcServer.Stop()
		}
	}()

	logger.Info("api ready", "addr", *listenAddr, "worker", *workerAddr)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}

func applyEnvFallback(target *string, envKey string) {
	if target == nil || *target != "" {
		return
	}
	if val := os.Getenv(envKey); val != "" {
		*target = val
	}
}
