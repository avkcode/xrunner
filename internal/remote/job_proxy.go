package remote

import (
	"context"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type jobProxy struct {
	xrunnerv1.UnimplementedJobServiceServer

	upstream xrunnerv1.JobServiceClient
}

func (p *jobProxy) SubmitJob(ctx context.Context, req *xrunnerv1.SubmitJobRequest) (*xrunnerv1.SubmitJobResponse, error) {
	return p.upstream.SubmitJob(ctx, req)
}

func (p *jobProxy) GetJob(ctx context.Context, req *xrunnerv1.GetJobRequest) (*xrunnerv1.Job, error) {
	return p.upstream.GetJob(ctx, req)
}

func (p *jobProxy) ListJobs(ctx context.Context, req *xrunnerv1.ListJobsRequest) (*xrunnerv1.ListJobsResponse, error) {
	return p.upstream.ListJobs(ctx, req)
}

func (p *jobProxy) StreamJobLogs(req *xrunnerv1.StreamJobLogsRequest, stream xrunnerv1.JobService_StreamJobLogsServer) error {
	up, err := p.upstream.StreamJobLogs(stream.Context(), req)
	if err != nil {
		return err
	}
	for {
		chunk, err := up.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				return nil
			}
			return err
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
}
