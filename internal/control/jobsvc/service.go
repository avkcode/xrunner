package jobsvc

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/control/store"
)

// Service provides the JobService gRPC server implementation.
type Service struct {
	xrunnerv1.UnimplementedJobServiceServer

	store   *store.Store
	worker  xrunnerv1.WorkerServiceClient
	logger  *slog.Logger
	clockFn func() time.Time

	dispatchGroup sync.WaitGroup

	logHeartbeatInterval time.Duration
}

// Config controls behavior of the job service.
type Config struct {
	WorkerTimeout time.Duration
}

// New creates a job service bound to the provided store and worker client.
func New(st *store.Store, worker xrunnerv1.WorkerServiceClient, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		store:   st,
		worker:  worker,
		logger:  logger,
		clockFn: time.Now,

		logHeartbeatInterval: 5 * time.Second,
	}
}

func (s *Service) SetLogHeartbeatInterval(d time.Duration) {
	s.logHeartbeatInterval = d
}

// Close waits for inflight dispatch routines.
func (s *Service) Close() {
	s.dispatchGroup.Wait()
}

// SubmitJob records a job then asynchronously dispatches it to the worker plane.
func (s *Service) SubmitJob(ctx context.Context, req *xrunnerv1.SubmitJobRequest) (*xrunnerv1.SubmitJobResponse, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	if req.GetWorkload() == nil {
		return nil, status.Error(codes.InvalidArgument, "workload is required")
	}
	job := buildJobFromRequest(req, s.clockFn())
	s.store.Put(job)
	s.dispatchGroup.Add(1)
	go s.dispatch(job.GetRef())
	return &xrunnerv1.SubmitJobResponse{Job: job}, nil
}

func buildJobFromRequest(req *xrunnerv1.SubmitJobRequest, now time.Time) *xrunnerv1.Job {
	jobID := uuid.NewString()
	var resource *xrunnerv1.ResourceHints
	if req.GetResource() != nil {
		resource = proto.Clone(req.GetResource()).(*xrunnerv1.ResourceHints)
	}
	var policy *xrunnerv1.PolicyOverrides
	if req.GetPolicy() != nil {
		policy = proto.Clone(req.GetPolicy()).(*xrunnerv1.PolicyOverrides)
	}
	job := &xrunnerv1.Job{
		Ref: &xrunnerv1.JobRef{
			TenantId: req.GetTenantId(),
			JobId:    jobID,
		},
		DisplayName: req.GetDisplayName(),
		Workload:    proto.Clone(req.GetWorkload()).(*xrunnerv1.Workload),
		Env:         cloneMap(req.GetEnv()),
		State:       xrunnerv1.JobState_JOB_STATE_ACCEPTED,
		CreateTime:  timestamppb.New(now),
		UpdateTime:  timestamppb.New(now),
		Resource:    resource,
		Policy:      policy,
		Sandbox:     cloneSandbox(req.GetSandbox()),
	}
	return job
}

func cloneMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func cloneSandbox(spec *xrunnerv1.SandboxSpec) *xrunnerv1.SandboxSpec {
	if spec == nil {
		return nil
	}
	return proto.Clone(spec).(*xrunnerv1.SandboxSpec)
}

func (s *Service) dispatch(ref *xrunnerv1.JobRef) {
	defer s.dispatchGroup.Done()
	ctx := context.Background()
	job, err := s.store.Get(ref)
	if err != nil {
		s.logger.Error("dispatch skipped", "job", ref.GetJobId(), "err", err)
		return
	}
	start := s.clockFn()
	_, err = s.store.Get(ref)
	if err != nil {
		s.logger.Error("job disappeared before dispatch", "job", ref.GetJobId(), "err", err)
		return
	}
	assignment := &xrunnerv1.JobAssignment{
		Ref:      proto.Clone(job.GetRef()).(*xrunnerv1.JobRef),
		Workload: proto.Clone(job.GetWorkload()).(*xrunnerv1.Workload),
		Env:      cloneMap(job.GetEnv()),
		Sandbox:  cloneSandbox(job.GetSandbox()),
	}
	if job.GetPolicy() != nil {
		assignment.Policy = proto.Clone(job.GetPolicy()).(*xrunnerv1.PolicyOverrides)
	}
	if job.GetResource() != nil {
		assignment.Resource = proto.Clone(job.GetResource()).(*xrunnerv1.ResourceHints)
	}
	job.State = xrunnerv1.JobState_JOB_STATE_RUNNING
	job.UpdateTime = timestamppb.New(start)
	s.store.Put(job)
	stream, runErr := s.worker.RunJob(ctx, assignment)
	end := s.clockFn()
	if runErr != nil {
		s.logger.Error("worker failed", "job", ref.GetJobId(), "err", runErr)
		s.applyFailure(ref, runErr.Error(), end)
		return
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			s.logger.Error("worker stream closed without terminal result", "job", ref.GetJobId())
			s.applyFailure(ref, "worker stream closed unexpectedly", s.clockFn())
			return
		}
		if err != nil {
			s.logger.Error("worker stream error", "job", ref.GetJobId(), "err", err)
			s.applyFailure(ref, err.Error(), s.clockFn())
			return
		}
		if len(resp.GetLogs()) > 0 {
			s.store.AppendLogs(ref, resp.GetLogs())
		}
		if resp.GetTerminal() {
			s.applyCompletion(resp, start, s.clockFn())
			return
		}
	}
}

func (s *Service) applyFailure(ref *xrunnerv1.JobRef, message string, now time.Time) {
	job, err := s.store.Get(ref)
	if err != nil {
		return
	}
	job.State = xrunnerv1.JobState_JOB_STATE_FAILED
	job.StatusMessage = message
	job.UpdateTime = timestamppb.New(now)
	s.store.Put(job)
	s.store.CloseLogStreams(ref)
}

func (s *Service) applyCompletion(resp *xrunnerv1.RunJobResponse, started, completed time.Time) {
	job, err := s.store.Get(resp.GetRef())
	if err != nil {
		return
	}
	job.State = resp.GetFinalState()
	job.ExitCode = resp.GetExitCode()
	job.StatusMessage = resp.GetStatusMessage()
	if resp.GetCompletedAt() != nil {
		job.UpdateTime = proto.Clone(resp.GetCompletedAt()).(*timestamppb.Timestamp)
	} else {
		job.UpdateTime = timestamppb.New(completed)
	}
	s.store.Put(job)
	s.store.CloseLogStreams(resp.GetRef())
}

// GetJob returns the job by reference.
func (s *Service) GetJob(ctx context.Context, req *xrunnerv1.GetJobRequest) (*xrunnerv1.Job, error) {
	job, err := s.store.Get(req.GetRef())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return job, nil
}

// ListJobs returns tenant jobs.
func (s *Service) ListJobs(ctx context.Context, req *xrunnerv1.ListJobsRequest) (*xrunnerv1.ListJobsResponse, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	jobs := s.store.List(req.GetTenantId())
	return &xrunnerv1.ListJobsResponse{Jobs: jobs}, nil
}

// StreamJobLogs replays captured log chunks.
func (s *Service) StreamJobLogs(req *xrunnerv1.StreamJobLogsRequest, stream xrunnerv1.JobService_StreamJobLogsServer) error {
	chunks, err := s.store.Logs(req.GetRef())
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	after := req.GetAfterOffset()
	lastOffset := after
	for _, chunk := range chunks {
		if chunk.GetOffset() <= after {
			continue
		}
		if off := chunk.GetOffset(); off > lastOffset {
			lastOffset = off
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
	if job, err := s.store.Get(req.GetRef()); err == nil {
		switch job.GetState() {
		case xrunnerv1.JobState_JOB_STATE_SUCCEEDED, xrunnerv1.JobState_JOB_STATE_FAILED:
			return nil
		}
	}
	subID, logCh := s.store.SubscribeLogs(req.GetRef())
	defer s.store.UnsubscribeLogs(req.GetRef(), subID)
	var heartbeat *time.Ticker
	if s.logHeartbeatInterval > 0 {
		heartbeat = time.NewTicker(s.logHeartbeatInterval)
		defer heartbeat.Stop()
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-tickerChan(heartbeat):
			if err := stream.Send(&xrunnerv1.LogChunk{
				Timestamp: timestamppb.Now(),
				Stream:    "heartbeat",
				Data:      nil,
				Offset:    lastOffset,
			}); err != nil {
				return err
			}
		case chunk, ok := <-logCh:
			if !ok {
				return nil
			}
			if chunk.GetOffset() <= after {
				continue
			}
			if off := chunk.GetOffset(); off > lastOffset {
				lastOffset = off
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
	}
}

func tickerChan(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}
