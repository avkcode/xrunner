package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	cliconfig "github.com/antonkrylov/xrunner/internal/cli/config"
	"github.com/antonkrylov/xrunner/internal/client"
)

type rootOptions struct {
	apiAddr     string
	timeout     time.Duration
	configPath  string
	contextName string
	config      *cliconfig.Config
}

func (r *rootOptions) prepare() error {
	resolved, err := client.ResolveConnection(r.configPath, r.contextName, r.apiAddr, r.timeout)
	if err != nil {
		return err
	}
	r.apiAddr = resolved.APIAddr
	r.timeout = resolved.Timeout
	r.config = resolved.Config
	return nil
}

func main() {
	opts := &rootOptions{}
	rootCmd := &cobra.Command{
		Use:   "xrunner",
		Short: "CLI for the xrunner control plane",
	}
	defaultConfig := os.Getenv("XRUNNER_CONFIG")
	if defaultConfig == "" {
		defaultConfig = cliconfig.DefaultConfigPath()
	}
	rootCmd.PersistentFlags().StringVar(&opts.configPath, "config", defaultConfig, "path to xrunner config file (default $HOME/.xrunner/config)")
	rootCmd.PersistentFlags().StringVar(&opts.contextName, "context", "", "context name within the config (used for API + ssh defaults; overrides currentContext)")
	rootCmd.PersistentFlags().StringVar(&opts.apiAddr, "api-addr", "", "JobService gRPC endpoint (overrides config)")
	rootCmd.PersistentFlags().DurationVar(&opts.timeout, "timeout", 0, "client timeout; defaults to config or 15s")
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// SSH subcommands manage their own connections (and may create new contexts during bootstrap),
		// so avoid forcing config context resolution here.
		for c := cmd; c != nil; c = c.Parent() {
			if c.Name() == "ssh" {
				return nil
			}
		}
		return opts.prepare()
	}

	rootCmd.AddCommand(newJobCmd(opts))
	rootCmd.AddCommand(newRunbookCmd(opts))
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newSSHCmd())

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func newJobCmd(root *rootOptions) *cobra.Command {
	jobCmd := &cobra.Command{
		Use:   "job",
		Short: "Job operations",
	}
	jobCmd.AddCommand(newJobSubmitCmd(root))
	jobCmd.AddCommand(newJobGetCmd(root))
	jobCmd.AddCommand(newJobLogsCmd(root))
	jobCmd.AddCommand(newJobListCmd(root))
	return jobCmd
}

func newJobSubmitCmd(root *rootOptions) *cobra.Command {
	opts := &jobSubmitFlags{}
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit a job",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.tenantID == "" {
				return fmt.Errorf("tenant is required")
			}
			envMap, err := parseEnv(opts.envVars)
			if err != nil {
				return err
			}
			if opts.toolBundle != "" {
				envKey := opts.toolEnvKey
				if strings.TrimSpace(envKey) == "" {
					envKey = "AGENT_TOOL_SPEC"
				}
				abs, err := filepath.Abs(opts.toolBundle)
				if err != nil {
					return fmt.Errorf("tool bundle: %w", err)
				}
				if _, err := os.Stat(abs); err != nil {
					return fmt.Errorf("tool bundle: %w", err)
				}
				if envMap == nil {
					envMap = map[string]string{}
				}
				envMap[envKey] = abs
			}
			stdinBytes, err := readOptionalFile(opts.stdinPath)
			if err != nil {
				return err
			}
			workload, err := opts.buildWorkload(stdinBytes)
			if err != nil {
				return err
			}
			sandbox, err := opts.buildSandbox()
			if err != nil {
				return err
			}
			resource := buildResourceHints(opts)
			req := &xrunnerv1.SubmitJobRequest{
				TenantId:    opts.tenantID,
				DisplayName: opts.displayName,
				Workload:    workload,
				Env:         envMap,
				Resource:    resource,
				Policy: &xrunnerv1.PolicyOverrides{
					AllowNetwork:        opts.allowNetwork,
					MaxExecutionSeconds: opts.maxSeconds,
				},
				Sandbox: sandbox,
			}
			ctx, cancel := context.WithTimeout(context.Background(), root.timeout)
			defer cancel()
			client, conn, err := root.dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := client.SubmitJob(ctx, req)
			if err != nil {
				return err
			}
			printJob(resp.GetJob())
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.tenantID, "tenant", "default", "tenant identifier")
	cmd.Flags().StringVar(&opts.displayName, "display-name", "", "human readable job name")
	cmd.Flags().StringVar(&opts.workloadKind, "workload", "inline", "workload type: inline|docker-run|docker-build")
	cmd.Flags().StringVar(&opts.executable, "cmd", "", "inline executable path")
	cmd.Flags().StringArrayVar(&opts.inlineArgs, "arg", nil, "inline command argument (repeatable)")
	cmd.Flags().StringArrayVar(&opts.envVars, "env", nil, "host environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringVar(&opts.toolBundle, "tool-bundle", "", "path to OpenAI-compatible tool bundle JSON; exported to the job environment")
	cmd.Flags().StringVar(&opts.toolEnvKey, "tool-env-var", "AGENT_TOOL_SPEC", "environment variable used to expose --tool-bundle")
	cmd.Flags().StringVar(&opts.stdinPath, "stdin-file", "", "path to file piped to stdin for inline workloads")
	cmd.Flags().Float64Var(&opts.cpuCores, "cpu", 0, "cpu cores hint")
	cmd.Flags().Int64Var(&opts.memoryMB, "memory-mb", 0, "memory hint in MB")
	cmd.Flags().BoolVar(&opts.needsGPU, "needs-gpu", false, "request GPU access")
	cmd.Flags().BoolVar(&opts.allowNetwork, "allow-network", false, "allow network egress policy override")
	cmd.Flags().Int64Var(&opts.maxSeconds, "max-seconds", 0, "override execution deadline in seconds")
	cmd.Flags().StringVar(&opts.sandboxType, "sandbox-type", "", "override sandbox type: nsjail|nsjail-docker|docker|none")
	cmd.Flags().StringVar(&opts.sandboxConfig, "sandbox-config", "", "nsjail config path for this job")
	cmd.Flags().StringVar(&opts.sandboxBin, "sandbox-bin", "", "sandbox binary override")
	cmd.Flags().StringArrayVar(&opts.sandboxBinds, "sandbox-bind", nil, "sandbox bind mount spec src:dst[:mode] (repeatable)")
	cmd.Flags().StringVar(&opts.sandboxWorkdir, "sandbox-workdir", "", "working directory inside sandbox")
	cmd.Flags().StringVar(&opts.sandboxLog, "sandbox-log", "", "log path for sandbox execution")
	cmd.Flags().StringArrayVar(&opts.sandboxExtra, "sandbox-extra-arg", nil, "extra sandbox binary argument (repeatable)")
	cmd.Flags().StringVar(&opts.containerImage, "container-image", "", "docker image for docker-run workload")
	cmd.Flags().StringArrayVar(&opts.containerCmd, "container-cmd", nil, "command/args executed inside container (repeatable)")
	cmd.Flags().StringArrayVar(&opts.containerEnv, "container-env", nil, "container environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.containerMount, "container-mount", nil, "docker bind mount src:dst[:mode] (repeatable)")
	cmd.Flags().StringVar(&opts.containerNetwork, "container-network", "", "docker network mode")
	cmd.Flags().StringVar(&opts.containerWorkdir, "container-workdir", "", "container working directory")
	cmd.Flags().StringVar(&opts.containerEntrypoint, "container-entrypoint", "", "override container entrypoint")
	cmd.Flags().BoolVar(&opts.containerKeep, "container-keep", false, "keep container after exit (defaults to auto-remove)")
	cmd.Flags().BoolVar(&opts.containerInteractive, "container-interactive", false, "attach stdin to container")
	cmd.Flags().BoolVar(&opts.containerTTY, "container-tty", false, "allocate TTY for container")
	cmd.Flags().BoolVar(&opts.containerDetach, "container-detach", false, "run container detached")
	cmd.Flags().StringArrayVar(&opts.containerExtra, "container-extra-arg", nil, "extra docker run argument (repeatable)")
	cmd.Flags().StringVar(&opts.buildContext, "build-context", "", "docker build context path or URL")
	cmd.Flags().StringVar(&opts.buildDockerfile, "dockerfile", "", "path to Dockerfile relative to context")
	cmd.Flags().StringArrayVar(&opts.buildTags, "tag", nil, "docker tag for build result (repeatable)")
	cmd.Flags().StringArrayVar(&opts.buildPlatforms, "platform", nil, "target platform for build (repeatable)")
	cmd.Flags().StringArrayVar(&opts.buildArgs, "build-arg", nil, "docker build argument KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.buildSecrets, "build-secret", nil, "build secret spec id=name,src=path (repeatable)")
	cmd.Flags().StringArrayVar(&opts.buildCacheFrom, "build-cache-from", nil, "cache-from spec (repeatable)")
	cmd.Flags().StringArrayVar(&opts.buildCacheTo, "build-cache-to", nil, "cache-to spec (repeatable)")
	cmd.Flags().BoolVar(&opts.buildPush, "build-push", false, "push image after build")
	cmd.Flags().BoolVar(&opts.buildLoad, "build-load", false, "load image into local docker")
	cmd.Flags().BoolVar(&opts.buildNoCache, "build-no-cache", false, "disable build cache")
	cmd.Flags().BoolVar(&opts.buildQuiet, "build-quiet", false, "suppress build output")
	cmd.Flags().StringVar(&opts.buildBuilder, "build-builder", "", "docker buildx builder name")
	cmd.Flags().BoolVar(&opts.buildUseBuildkit, "build-use-buildkit", true, "force BuildKit backend for docker build")
	cmd.Flags().StringArrayVar(&opts.buildComposeFiles, "build-compose-file", nil, "compose file passed to build (repeatable)")
	cmd.Flags().StringArrayVar(&opts.buildExtra, "build-extra-arg", nil, "extra docker buildx argument (repeatable)")
	return cmd
}

func newJobGetCmd(root *rootOptions) *cobra.Command {
	var tenantID string
	cmd := &cobra.Command{
		Use:   "get <job-id>",
		Short: "Fetch a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), root.timeout)
			defer cancel()
			client, conn, err := root.dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			job, err := client.GetJob(ctx, &xrunnerv1.GetJobRequest{Ref: &xrunnerv1.JobRef{TenantId: tenantID, JobId: args[0]}})
			if err != nil {
				return err
			}
			printJob(job)
			return nil
		},
	}
	cmd.Flags().StringVar(&tenantID, "tenant", "default", "tenant identifier")
	return cmd
}

func newJobLogsCmd(root *rootOptions) *cobra.Command {
	var tenantID string
	var raw bool
	var streamFilter string
	var reconnect bool
	var statusInterval time.Duration
	cmd := &cobra.Command{
		Use:   "logs <job-id>",
		Short: "Stream job logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			filter := strings.ToLower(strings.TrimSpace(streamFilter))
			ref := &xrunnerv1.JobRef{TenantId: tenantID, JobId: args[0]}

			var lastOffset uint64
			lastDataAt := time.Now()
			lastStatusAt := time.Time{}

			nextBackoff := 250 * time.Millisecond
			maxBackoff := 5 * time.Second

			for {
				if ctx.Err() != nil {
					return nil
				}

				dialCtx, cancelDial := context.WithTimeout(context.Background(), root.timeout)
				client, conn, err := root.dial(dialCtx)
				cancelDial()
				if err != nil {
					return err
				}

				stream, err := client.StreamJobLogs(ctx, &xrunnerv1.StreamJobLogsRequest{
					Ref:         ref,
					AfterOffset: lastOffset,
				})
				if err != nil {
					_ = conn.Close()
					return err
				}

				// reset reconnect backoff after a successful connect+start.
				nextBackoff = 250 * time.Millisecond

				for {
					if ctx.Err() != nil {
						_ = conn.Close()
						return nil
					}

					if statusInterval > 0 && time.Since(lastDataAt) >= statusInterval && time.Since(lastStatusAt) >= statusInterval {
						lastStatusAt = time.Now()
						_, _ = fmt.Fprintf(os.Stderr, "connected (idle) job=%s offset=%d\n", ref.GetJobId(), lastOffset)
					}

					chunk, err := stream.Recv()
					if err == io.EOF {
						_ = conn.Close()
						return nil
					}
					if err != nil {
						_ = conn.Close()
						if !reconnect || !isTransient(err) {
							return err
						}
						_, _ = fmt.Fprintf(os.Stderr, "stream interrupted (%v); reconnecting in %s\n", err, nextBackoff)
						timer := time.NewTimer(nextBackoff)
						select {
						case <-ctx.Done():
							timer.Stop()
							return nil
						case <-timer.C:
						}
						nextBackoff *= 2
						if nextBackoff > maxBackoff {
							nextBackoff = maxBackoff
						}
						break // reconnect outer loop
					}

					if chunk.GetOffset() <= lastOffset {
						continue
					}
					lastOffset = chunk.GetOffset()

					streamName := strings.ToLower(strings.TrimSpace(chunk.GetStream()))
					if streamName == "heartbeat" && len(chunk.GetData()) == 0 {
						continue
					}

					if filter != "" && streamName != filter {
						continue
					}

					if len(chunk.GetData()) > 0 {
						lastDataAt = time.Now()
					}

					if raw {
						if _, err := os.Stdout.Write(chunk.GetData()); err != nil {
							_ = conn.Close()
							return err
						}
						continue
					}

					ts := "<unknown>"
					if chunk.GetTimestamp() != nil {
						ts = chunk.GetTimestamp().AsTime().Format(time.RFC3339)
					}
					if _, err := fmt.Fprintf(os.Stdout, "[%s][%s] %s", ts, chunk.GetStream(), chunk.GetData()); err != nil {
						_ = conn.Close()
						return err
					}
				}
			}
		},
	}
	cmd.Flags().StringVar(&tenantID, "tenant", "default", "tenant identifier")
	cmd.Flags().BoolVar(&raw, "raw", false, "print log bytes without prefixes (best for interactive output)")
	cmd.Flags().StringVar(&streamFilter, "stream", "", "filter by stream name (stdout|stderr); empty streams all")
	cmd.Flags().BoolVar(&reconnect, "reconnect", true, "reconnect on transient stream errors")
	cmd.Flags().DurationVar(&statusInterval, "status-interval", 0, "print an idle connection status line to stderr at this interval (0 disables)")
	return cmd
}

func isTransient(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted:
		return true
	default:
		return false
	}
}

func newJobListCmd(root *rootOptions) *cobra.Command {
	var tenantID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs for a tenant",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), root.timeout)
			defer cancel()
			client, conn, err := root.dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := client.ListJobs(ctx, &xrunnerv1.ListJobsRequest{TenantId: tenantID})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "JOB ID\tSTATE\tEXIT\tUPDATED")
			for _, job := range resp.GetJobs() {
				updated := "<unknown>"
				if job.GetUpdateTime() != nil {
					updated = job.GetUpdateTime().AsTime().Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", job.GetRef().GetJobId(), job.GetState().String(), job.GetExitCode(), updated)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&tenantID, "tenant", "default", "tenant identifier")
	return cmd
}

func readOptionalFile(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	return os.ReadFile(path)
}

func parseEnv(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid env %q", item)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

func printJob(job *xrunnerv1.Job) {
	if job == nil {
		fmt.Println("<nil job>")
		return
	}
	fmt.Printf("Job %s (%s)\n", job.GetRef().GetJobId(), job.GetDisplayName())
	fmt.Printf("  State: %s\n", job.GetState())
	fmt.Printf("  Exit: %d\n", job.GetExitCode())
	fmt.Printf("  Created: %s\n", formatTS(job.GetCreateTime()))
	fmt.Printf("  Updated: %s\n", formatTS(job.GetUpdateTime()))
	if msg := job.GetStatusMessage(); msg != "" {
		fmt.Printf("  Message: %s\n", msg)
	}
}

func formatTS(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "<unknown>"
	}
	return ts.AsTime().Format(time.RFC3339)
}

type jobSubmitFlags struct {
	tenantID     string
	displayName  string
	workloadKind string

	executable string
	inlineArgs []string
	stdinPath  string

	envVars      []string
	toolBundle   string
	toolEnvKey   string
	cpuCores     float64
	memoryMB     int64
	needsGPU     bool
	allowNetwork bool
	maxSeconds   int64

	sandboxType    string
	sandboxConfig  string
	sandboxBin     string
	sandboxBinds   []string
	sandboxWorkdir string
	sandboxLog     string
	sandboxExtra   []string

	containerImage       string
	containerCmd         []string
	containerEnv         []string
	containerMount       []string
	containerNetwork     string
	containerWorkdir     string
	containerEntrypoint  string
	containerKeep        bool
	containerInteractive bool
	containerTTY         bool
	containerDetach      bool
	containerExtra       []string

	buildContext      string
	buildDockerfile   string
	buildTags         []string
	buildPlatforms    []string
	buildArgs         []string
	buildSecrets      []string
	buildCacheFrom    []string
	buildCacheTo      []string
	buildPush         bool
	buildLoad         bool
	buildNoCache      bool
	buildQuiet        bool
	buildBuilder      string
	buildUseBuildkit  bool
	buildComposeFiles []string
	buildExtra        []string
}

func (o *jobSubmitFlags) buildWorkload(stdin []byte) (*xrunnerv1.Workload, error) {
	switch strings.ToLower(o.workloadKind) {
	case "", "inline":
		if strings.TrimSpace(o.executable) == "" {
			return nil, fmt.Errorf("--cmd is required for inline workloads")
		}
		return &xrunnerv1.Workload{
			Kind: &xrunnerv1.Workload_Inline{
				Inline: &xrunnerv1.InlineWorkload{
					Executable: o.executable,
					Args:       append([]string(nil), o.inlineArgs...),
					Stdin:      stdin,
				},
			},
		}, nil
	case "docker-run":
		if strings.TrimSpace(o.containerImage) == "" {
			return nil, fmt.Errorf("--container-image is required for docker-run workloads")
		}
		containerEnv, err := parseEnv(o.containerEnv)
		if err != nil {
			return nil, err
		}
		return &xrunnerv1.Workload{
			Kind: &xrunnerv1.Workload_DockerRun{
				DockerRun: &xrunnerv1.DockerRunWorkload{
					Image:         o.containerImage,
					Command:       append([]string(nil), o.containerCmd...),
					ContainerEnv:  containerEnv,
					Mounts:        append([]string(nil), o.containerMount...),
					Workdir:       o.containerWorkdir,
					Network:       o.containerNetwork,
					KeepContainer: o.containerKeep,
					Interactive:   o.containerInteractive,
					Tty:           o.containerTTY,
					Detach:        o.containerDetach,
					Entrypoint:    o.containerEntrypoint,
					ExtraArgs:     append([]string(nil), o.containerExtra...),
				},
			},
		}, nil
	case "docker-build":
		if strings.TrimSpace(o.buildContext) == "" {
			return nil, fmt.Errorf("--build-context is required for docker-build workloads")
		}
		return &xrunnerv1.Workload{
			Kind: &xrunnerv1.Workload_DockerBuild{
				DockerBuild: &xrunnerv1.DockerBuildWorkload{
					ContextDir:   o.buildContext,
					Dockerfile:   o.buildDockerfile,
					Tags:         append([]string(nil), o.buildTags...),
					Platforms:    append([]string(nil), o.buildPlatforms...),
					BuildArgs:    append([]string(nil), o.buildArgs...),
					Secrets:      append([]string(nil), o.buildSecrets...),
					CacheFrom:    append([]string(nil), o.buildCacheFrom...),
					CacheTo:      append([]string(nil), o.buildCacheTo...),
					Push:         o.buildPush,
					Load:         o.buildLoad,
					NoCache:      o.buildNoCache,
					Quiet:        o.buildQuiet,
					Builder:      o.buildBuilder,
					UseBuildkit:  o.buildUseBuildkit,
					ComposeFiles: append([]string(nil), o.buildComposeFiles...),
					ExtraArgs:    append([]string(nil), o.buildExtra...),
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported workload type %q", o.workloadKind)
	}
}

func (o *jobSubmitFlags) buildSandbox() (*xrunnerv1.SandboxSpec, error) {
	if o.sandboxType == "" && o.sandboxConfig == "" && len(o.sandboxBinds) == 0 && o.sandboxBin == "" && len(o.sandboxExtra) == 0 && o.sandboxWorkdir == "" && o.sandboxLog == "" {
		return nil, nil
	}
	sTypeString := o.sandboxType
	if sTypeString == "" {
		sTypeString = "nsjail"
	}
	sType, err := parseSandboxTypeFlag(sTypeString)
	if err != nil {
		return nil, err
	}
	return &xrunnerv1.SandboxSpec{
		Type:         sType,
		NsjailConfig: o.sandboxConfig,
		SandboxBin:   o.sandboxBin,
		Binds:        append([]string(nil), o.sandboxBinds...),
		Workdir:      o.sandboxWorkdir,
		LogPath:      o.sandboxLog,
		ExtraArgs:    append([]string(nil), o.sandboxExtra...),
	}, nil
}

func buildResourceHints(opts *jobSubmitFlags) *xrunnerv1.ResourceHints {
	if opts.cpuCores == 0 && opts.memoryMB == 0 && !opts.needsGPU {
		return nil
	}
	return &xrunnerv1.ResourceHints{
		CpuCores:    opts.cpuCores,
		MemoryBytes: opts.memoryMB * 1024 * 1024,
		NeedsGpu:    opts.needsGPU,
	}
}

func parseSandboxTypeFlag(val string) (xrunnerv1.SandboxType, error) {
	if val == "" {
		return xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, nil
	}
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "none":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, nil
	case "nsjail":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_NSJAIL, nil
	case "nsjail-docker":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_NSJAIL_DOCKER, nil
	case "docker":
		return xrunnerv1.SandboxType_SANDBOX_TYPE_DOCKER, nil
	default:
		return xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, errors.New("invalid sandbox type: " + val)
	}
}

func (r *rootOptions) dial(ctx context.Context) (xrunnerv1.JobServiceClient, *grpc.ClientConn, error) {
	return client.DialJobService(ctx, r.apiAddr, client.DialInsecure)
}
