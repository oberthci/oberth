package sourceplugin

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"net"
	"os"
	"path/filepath"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type rpcServer struct {
	artifact.UnimplementedArtifactServiceServer
	client *Client
}

func (s *rpcServer) Load(ctx context.Context, r *artifact.LoadArtifactRequest) (*artifact.LoadArtifactResponse, error) {
	if r == nil {
		return nil, status.Error(codes.InvalidArgument, reasonInput)
	}
	if err := s.client.Load(ctx, r.InputArtifact, r.Path); err != nil {
		return nil, status.Error(loadCode(ctx, err), err.Error())
	}
	return &artifact.LoadArtifactResponse{Success: true}, nil
}
func (*rpcServer) Save(context.Context, *artifact.SaveArtifactRequest) (*artifact.SaveArtifactResponse, error) {
	return nil, status.Error(codes.Unimplemented, reasonOutput)
}
func (*rpcServer) Delete(context.Context, *artifact.DeleteArtifactRequest) (*artifact.DeleteArtifactResponse, error) {
	return nil, status.Error(codes.Unimplemented, reasonOutput)
}
func (*rpcServer) IsDirectory(_ context.Context, r *artifact.IsDirectoryRequest) (*artifact.IsDirectoryResponse, error) {
	if r == nil {
		return nil, status.Error(codes.InvalidArgument, reasonInput)
	}
	if _, err := input(r.Artifact); err != nil {
		return nil, status.Error(codes.InvalidArgument, reasonInput)
	}
	return &artifact.IsDirectoryResponse{IsDirectory: false}, nil
}
func Serve(ctx context.Context, socket string, client *Client) error {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || client == nil {
		return failure(reasonSocket)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return failure(reasonSocket)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return failure(reasonSocket)
	}
	defer func() { _ = listener.Close() }()
	if err = os.Chmod(socket, 0600); err != nil {
		return failure(reasonSocket)
	}
	rpc := grpc.NewServer(grpc.WaitForHandlers(true))
	artifact.RegisterArtifactServiceServer(rpc, &rpcServer{client: client})
	stop := context.AfterFunc(ctx, rpc.Stop)
	defer stop()
	defer rpc.Stop()
	if err = rpc.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return failure(reasonSocket)
	}
	return nil
}

func loadCode(ctx context.Context, err error) codes.Code {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return codes.DeadlineExceeded
	}
	if ctx.Err() != nil {
		return codes.Canceled
	}
	var refusal *loadRefusal
	if errors.As(err, &refusal) {
		return codes.FailedPrecondition
	}
	var failure *failureError
	if errors.As(err, &failure) {
		switch failure.reason {
		case reasonInput:
			return codes.InvalidArgument
		case reasonDenied:
			return codes.PermissionDenied
		case reasonDisabled, reasonConfig, reasonIdentity, reasonFile:
			return codes.FailedPrecondition
		case reasonReceipt:
			return codes.DataLoss
		}
	}
	return codes.Unavailable
}

func input(a *artifact.Artifact) (sourcedelivery.LoadRequest, error) {
	if a == nil || a.Plugin == nil || a.Plugin.Name != "oberth" {
		return sourcedelivery.LoadRequest{}, failure(reasonInput)
	}
	d, err := sourcedelivery.DecodeDescriptor([]byte(a.Plugin.Configuration))
	if err != nil || a.Plugin.Key == "" || len(a.Plugin.Key) > 128 {
		return sourcedelivery.LoadRequest{}, failure(reasonInput)
	}
	return sourcedelivery.LoadRequest{Locator: a.Plugin.Key, Descriptor: d}, nil
}
