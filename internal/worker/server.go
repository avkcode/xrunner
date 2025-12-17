package worker

import (
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

// Server implements the WorkerService gRPC server.
type Server struct {
	xrunnerv1.UnimplementedWorkerServiceServer
	runner *Runner
	logger *slog.Logger
}

// NewServer wires a Runner into a gRPC server implementation.
func NewServer(runner *Runner, logger *slog.Logger) *Server {
	return &Server{runner: runner, logger: logger}
}

// RunJob satisfies the WorkerService contract with streaming log forwarding.
func (s *Server) RunJob(assignment *xrunnerv1.JobAssignment, stream xrunnerv1.WorkerService_RunJobServer) error {
	if assignment == nil {
		return status.Error(codes.InvalidArgument, "assignment is required")
	}
	forward := func(chunk *xrunnerv1.LogChunk) error {
		return stream.Send(&xrunnerv1.RunJobResponse{
			Ref:  assignment.GetRef(),
			Logs: []*xrunnerv1.LogChunk{chunk},
		})
	}
	resp, err := s.runner.Execute(stream.Context(), assignment, forward)
	if err != nil {
		s.logger.Error("job failed", "job", assignment.GetRef().GetJobId(), "err", err)
		return err
	}
	resp.Terminal = true
	return stream.Send(resp)
}
