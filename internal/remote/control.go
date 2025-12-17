package remote

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type controlService struct {
	xrunnerv1.UnimplementedRemoteControlServiceServer

	cfg Config
}

func (s *controlService) Ping(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *controlService) Version(context.Context, *emptypb.Empty) (*xrunnerv1.RemoteVersionResponse, error) {
	return &xrunnerv1.RemoteVersionResponse{
		Version:   s.cfg.Version,
		Commit:    s.cfg.Commit,
		BuildTime: s.cfg.BuildTime,
	}, nil
}
