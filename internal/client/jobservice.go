package client

import (
	"context"
	"time"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type DialSecurityMode int

const (
	DialInsecure DialSecurityMode = iota
	DialTLS
)

func DialJobService(ctx context.Context, addr string, mode DialSecurityMode, dialOptions ...grpc.DialOption) (xrunnerv1.JobServiceClient, *grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	switch mode {
	case DialTLS:
		creds = credentials.NewClientTLSFromCert(nil, "")
	default:
		creds = insecure.NewCredentials()
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// Be conservative: some servers enforce a large MinTime (e.g. 5m) and will send
			// GOAWAY "too_many_pings" for aggressive keepalive settings.
			Time:                5 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  250 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   5 * time.Second,
			},
			MinConnectTimeout: 10 * time.Second,
		}),
	}
	opts = append(opts, dialOptions...)

	conn, err := grpc.DialContext(ctx, addr, opts...)
	if err != nil {
		return nil, nil, err
	}
	return xrunnerv1.NewJobServiceClient(conn), conn, nil
}
