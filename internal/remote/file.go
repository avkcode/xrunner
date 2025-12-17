package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type fileService struct {
	xrunnerv1.UnimplementedRemoteFileServiceServer

	root         string
	unsafeRootFS bool
}

func (s *fileService) ReadFile(ctx context.Context, req *xrunnerv1.ReadFileRequest) (*xrunnerv1.ReadFileResponse, error) {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &xrunnerv1.ReadFileResponse{Data: data}, nil
}

func (s *fileService) WriteFileAtomic(ctx context.Context, req *xrunnerv1.WriteFileRequest) (*emptypb.Empty, error) {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mode := os.FileMode(req.GetMode())
	if mode == 0 {
		mode = 0o644
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".xrunner-write-*")
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return nil, status.Error(codes.Internal, err.Error())
	}
	if _, err := tmp.Write(req.GetData()); err != nil {
		_ = tmp.Close()
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := tmp.Close(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *fileService) resolve(requestPath string) (string, error) {
	raw := strings.TrimSpace(requestPath)
	if raw == "" {
		return "", status.Error(codes.InvalidArgument, "path is required")
	}
	if s.unsafeRootFS {
		return filepath.Clean(raw), nil
	}
	root := s.root
	if root == "" {
		return "", status.Error(codes.FailedPrecondition, "workspace root is not configured")
	}
	root = filepath.Clean(root)
	var abs string
	if filepath.IsAbs(raw) {
		abs = filepath.Clean(raw)
	} else {
		abs = filepath.Join(root, raw)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", status.Error(codes.PermissionDenied, fmt.Sprintf("path %q escapes workspace root", raw))
	}
	return abs, nil
}
