package worker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

// Runner executes any workload (inline, docker run, docker build) possibly wrapped in a sandbox.
type Runner struct {
	WorkspaceRoot       string
	AllowUnsafeFallback bool
	Logger              *slog.Logger
	DefaultSandbox      *xrunnerv1.SandboxSpec
}

// Execute runs the assignment and produces a RunJobResponse.
func (r *Runner) Execute(ctx context.Context, assignment *xrunnerv1.JobAssignment, onChunk func(*xrunnerv1.LogChunk) error) (*xrunnerv1.RunJobResponse, error) {
	if assignment == nil || assignment.GetRef() == nil {
		return nil, status.Error(codes.InvalidArgument, "assignment and ref required")
	}
	workload := assignment.GetWorkload()
	if workload == nil {
		return nil, status.Error(codes.InvalidArgument, "workload is required")
	}

	workspace, err := r.prepareWorkspace(assignment)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	hostEnv := r.composeHostEnv(assignment.GetEnv())

	var (
		baseCmd        []string
		stdinPayload   []byte
		envAugment     []string
		workloadDetail string
	)

	switch typed := workload.Kind.(type) {
	case *xrunnerv1.Workload_Inline:
		inline := typed.Inline
		if inline.GetExecutable() == "" {
			return nil, status.Error(codes.InvalidArgument, "inline executable is required")
		}
		baseCmd = append([]string{inline.GetExecutable()}, inline.GetArgs()...)
		stdinPayload = inline.GetStdin()
		workloadDetail = "inline"
	case *xrunnerv1.Workload_DockerRun:
		baseCmd, err = buildDockerRunCommand(typed.DockerRun)
		if err != nil {
			return nil, err
		}
		workloadDetail = fmt.Sprintf("docker-run %s", typed.DockerRun.GetImage())
	case *xrunnerv1.Workload_DockerBuild:
		baseCmd, envAugment, err = buildDockerBuildCommand(typed.DockerBuild)
		if err != nil {
			return nil, err
		}
		workloadDetail = "docker-build"
	default:
		return nil, status.Error(codes.Unimplemented, "workload not supported")
	}

	hostEnv = append(hostEnv, envAugment...)

	cmd, sandboxType, err := r.wrapWithSandbox(ctx, assignment.GetSandbox(), baseCmd, hostEnv, workspace)
	if err != nil {
		return nil, err
	}
	if len(stdinPayload) > 0 {
		cmd.Stdin = bytes.NewReader(stdinPayload)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, status.Errorf(codes.Internal, "start: %v", err)
	}

	var chunksMu sync.Mutex
	chunks := make([]*xrunnerv1.LogChunk, 0)
	wg := sync.WaitGroup{}
	forward := func(chunk *xrunnerv1.LogChunk) {
		if onChunk == nil {
			return
		}
		if err := onChunk(chunk); err != nil {
			r.Logger.Warn("forward log chunk failed", "job", assignment.GetRef().GetJobId(), "err", err)
		}
	}
	wg.Add(2)
	go r.collectStream(&wg, &chunksMu, &chunks, "stdout", stdoutPipe, forward)
	go r.collectStream(&wg, &chunksMu, &chunks, "stderr", stderrPipe, forward)

	wg.Wait()
	runErr := cmd.Wait()
	completed := time.Now()
	exitCode := exitCodeFromError(runErr)
	statusMsg := ""
	if runErr != nil {
		statusMsg = runErr.Error()
	}
	r.Logger.Info("job finished",
		"job", assignment.GetRef().GetJobId(),
		"workload", workloadDetail,
		"sandbox", sandboxType.String(),
		"exit", exitCode,
	)

	return &xrunnerv1.RunJobResponse{
		Ref:           assignment.GetRef(),
		FinalState:    finalizeState(exitCode),
		ExitCode:      int32(exitCode),
		StatusMessage: statusMsg,
		StartedAt:     timestamppb.New(started),
		CompletedAt:   timestamppb.New(completed),
		Logs:          chunks,
	}, nil
}

func (r *Runner) prepareWorkspace(assignment *xrunnerv1.JobAssignment) (string, error) {
	jobID := assignment.GetRef().GetJobId()
	workspace := filepath.Join(r.WorkspaceRoot, jobID)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", err
	}
	return workspace, nil
}

func (r *Runner) composeHostEnv(jobEnv map[string]string) []string {
	env := os.Environ()
	for k, v := range jobEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

func (r *Runner) wrapWithSandbox(ctx context.Context, sandbox *xrunnerv1.SandboxSpec, baseCmd, env []string, workspace string) (*exec.Cmd, xrunnerv1.SandboxType, error) {
	effective := sandbox
	if effective == nil {
		effective = r.DefaultSandbox
	}
	if effective == nil || effective.GetType() == xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED {
		cmd := exec.CommandContext(ctx, baseCmd[0], baseCmd[1:]...)
		cmd.Env = env
		cmd.Dir = workspace
		return cmd, xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, nil
	}
	switch effective.GetType() {
	case xrunnerv1.SandboxType_SANDBOX_TYPE_DOCKER:
		cmd := exec.CommandContext(ctx, baseCmd[0], baseCmd[1:]...)
		cmd.Env = env
		cmd.Dir = workspace
		return cmd, xrunnerv1.SandboxType_SANDBOX_TYPE_DOCKER, nil
	case xrunnerv1.SandboxType_SANDBOX_TYPE_NSJAIL, xrunnerv1.SandboxType_SANDBOX_TYPE_NSJAIL_DOCKER:
		bin := effective.GetSandboxBin()
		if bin == "" {
			bin = "nsjail"
		}
		nsjailPath, err := exec.LookPath(bin)
		if err != nil {
			if !r.AllowUnsafeFallback {
				return nil, xrunnerv1.SandboxType_SANDBOX_TYPE_UNSPECIFIED, status.Errorf(codes.FailedPrecondition, "sandbox binary %s not found", bin)
			}
			r.Logger.Warn("sandbox unavailable, falling back to host execution", "bin", bin)
			return r.wrapWithSandbox(ctx, nil, baseCmd, env, workspace)
		}
		args := make([]string, 0)
		if cfg := effective.GetNsjailConfig(); cfg != "" {
			args = append(args, "--config", cfg)
		}
		if wd := effective.GetWorkdir(); wd != "" {
			args = append(args, "--cwd", wd)
		} else if workspace != "" {
			args = append(args, "--cwd", workspace)
		}
		for _, bind := range effective.GetBinds() {
			args = append(args, "--bindmount", bind)
		}
		if logPath := effective.GetLogPath(); logPath != "" {
			args = append(args, "--log", logPath)
		}
		args = append(args, effective.GetExtraArgs()...)
		args = append(args, "--")
		args = append(args, baseCmd...)
		cmd := exec.CommandContext(ctx, nsjailPath, args...)
		cmd.Env = env
		cmd.Dir = workspace
		return cmd, effective.GetType(), nil
	default:
		return r.wrapWithSandbox(ctx, nil, baseCmd, env, workspace)
	}
}

func buildDockerRunCommand(cfg *xrunnerv1.DockerRunWorkload) ([]string, error) {
	if cfg == nil {
		return nil, status.Error(codes.InvalidArgument, "docker_run requires configuration")
	}
	if strings.TrimSpace(cfg.GetImage()) == "" {
		return nil, status.Error(codes.InvalidArgument, "docker_run.image is required")
	}
	args := []string{"docker", "run"}
	if !cfg.GetKeepContainer() {
		args = append(args, "--rm")
	}
	if cfg.GetInteractive() {
		args = append(args, "-i")
	}
	if cfg.GetTty() {
		args = append(args, "-t")
	}
	if cfg.GetDetach() {
		args = append(args, "-d")
	}
	if cfg.GetWorkdir() != "" {
		args = append(args, "--workdir", cfg.GetWorkdir())
	}
	if cfg.GetNetwork() != "" {
		args = append(args, "--network", cfg.GetNetwork())
	}
	for _, mount := range cfg.GetMounts() {
		args = append(args, "-v", mount)
	}
	for key, value := range cfg.GetContainerEnv() {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}
	if ep := strings.TrimSpace(cfg.GetEntrypoint()); ep != "" {
		args = append(args, "--entrypoint", ep)
	}
	args = append(args, cfg.GetExtraArgs()...)
	args = append(args, cfg.GetImage())
	args = append(args, cfg.GetCommand()...)
	return args, nil
}

func buildDockerBuildCommand(cfg *xrunnerv1.DockerBuildWorkload) ([]string, []string, error) {
	if cfg == nil {
		return nil, nil, status.Error(codes.InvalidArgument, "docker_build requires configuration")
	}
	contextDir := cfg.GetContextDir()
	if strings.TrimSpace(contextDir) == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "docker_build.context_dir is required")
	}
	args := []string{"docker", "buildx", "build"}
	if cfg.GetDockerfile() != "" {
		args = append(args, "-f", cfg.GetDockerfile())
	}
	for _, tag := range cfg.GetTags() {
		args = append(args, "-t", tag)
	}
	for _, platform := range cfg.GetPlatforms() {
		args = append(args, "--platform", platform)
	}
	for _, buildArg := range cfg.GetBuildArgs() {
		args = append(args, "--build-arg", buildArg)
	}
	for _, secret := range cfg.GetSecrets() {
		args = append(args, "--secret", secret)
	}
	for _, cacheFrom := range cfg.GetCacheFrom() {
		args = append(args, "--cache-from", cacheFrom)
	}
	for _, cacheTo := range cfg.GetCacheTo() {
		args = append(args, "--cache-to", cacheTo)
	}
	if cfg.GetPush() {
		args = append(args, "--push")
	}
	if cfg.GetLoad() {
		args = append(args, "--load")
	}
	if cfg.GetNoCache() {
		args = append(args, "--no-cache")
	}
	if cfg.GetQuiet() {
		args = append(args, "--quiet")
	}
	if cfg.GetBuilder() != "" {
		args = append(args, "--builder", cfg.GetBuilder())
	}
	for _, composeFile := range cfg.GetComposeFiles() {
		args = append(args, "--compose-file", composeFile)
	}
	args = append(args, cfg.GetExtraArgs()...)
	args = append(args, contextDir)

	envAugment := make([]string, 0)
	if cfg.GetUseBuildkit() {
		envAugment = append(envAugment, "DOCKER_BUILDKIT=1")
	}
	return args, envAugment, nil
}

func (r *Runner) collectStream(
	wg *sync.WaitGroup,
	mu *sync.Mutex,
	chunks *[]*xrunnerv1.LogChunk,
	stream string,
	pipe io.Reader,
	forward func(*xrunnerv1.LogChunk),
) {
	defer wg.Done()
	reader := bufio.NewReader(pipe)
	for {
		data, err := reader.ReadBytes('\n')
		if len(data) > 0 {
			mu.Lock()
			chunk := &xrunnerv1.LogChunk{
				Timestamp: timestamppb.Now(),
				Stream:    stream,
				Data:      append([]byte{}, data...),
			}
			*chunks = append(*chunks, chunk)
			mu.Unlock()
			if forward != nil {
				forward(chunk)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.Logger.Error("log read", "stream", stream, "err", err)
			}
			return
		}
	}
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(interface{ ExitStatus() int }); ok {
			return status.ExitStatus()
		}
	}
	return 1
}

func finalizeState(code int) xrunnerv1.JobState {
	if code == 0 {
		return xrunnerv1.JobState_JOB_STATE_SUCCEEDED
	}
	return xrunnerv1.JobState_JOB_STATE_FAILED
}
