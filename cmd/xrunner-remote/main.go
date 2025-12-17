package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/antonkrylov/xrunner/internal/remote"
)

var (
	version   = "dev"
	commit    = ""
	buildTime = ""
)

func main() {
	var listen string
	var upstream string
	var workspaceRoot string
	var unsafeRootFS bool
	var upstreamTLS bool
	var sessionsDir string
	var logLevel string
	var verbose bool

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "xrunner-remote (%s)\n\n", version)
		fmt.Fprintf(out, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.StringVar(&listen, "listen", "127.0.0.1:7337", "listen address for xrunner-remote gRPC server")
	flag.StringVar(&upstream, "upstream", "127.0.0.1:50051", "upstream JobService address")
	flag.BoolVar(&upstreamTLS, "upstream-tls", false, "use TLS when dialing upstream JobService")
	flag.StringVar(&workspaceRoot, "workspace-root", "/", "workspace root for remote file operations")
	flag.BoolVar(&unsafeRootFS, "unsafe-allow-rootfs", true, "allow remote file operations on arbitrary paths")
	flag.StringVar(&sessionsDir, "sessions-dir", "", "directory used to persist session logs (default ~/.xrunner/sessions)")
	flag.StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.BoolVar(&verbose, "verbose", false, "enable verbose debug logging (same as -log-level=debug)")

	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(0)
	}

	flag.Parse()

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	} else {
		switch l := strings.ToLower(strings.TrimSpace(logLevel)); l {
		case "debug":
			level = slog.LevelDebug
		case "info", "":
			level = slog.LevelInfo
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		default:
			// Keep it user-friendly: warn and continue with info.
			log.Printf("unknown -log-level=%q (expected debug|info|warn|error); defaulting to info", logLevel)
			level = slog.LevelInfo
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv, err := remote.New(remote.Config{
		ListenAddr:    listen,
		UpstreamAddr:  upstream,
		UpstreamTLS:   upstreamTLS,
		WorkspaceRoot: workspaceRoot,
		UnsafeRootFS:  unsafeRootFS,
		SessionsDir:   sessionsDir,
		Version:       version,
		Commit:        commit,
		BuildTime:     buildTime,
		Logger:        logger,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	srv.Stop()
}
