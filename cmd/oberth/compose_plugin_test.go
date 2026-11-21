package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type pluginRequest struct {
	Locator, TokenFile, Destination string
	Dial                          func(context.Context, string, string) (net.Conn, error)
}

func pluginSocket(t *testing.T) string {
	t.Helper()
	// The kernel bounds Unix socket paths independently of Go test's temp prefix.
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "op-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "source.sock")
}

func loadPluginArtifact(t *testing.T, socket string, q pluginRequest) (*artifact.LoadArtifactResponse, error) {
	t.Helper()
	connection, err := grpc.NewClient("passthrough:///source", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	op, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: q.Locator, Configuration: `{"version":1,"kind":"main"}`}}
	return artifact.NewArtifactServiceClient(connection).Load(op, &artifact.LoadArtifactRequest{InputArtifact: input, Path: q.Destination}, grpc.WaitForReady(true))
}

func joinPlugin(t *testing.T, command *exec.Cmd, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	defer cancel()
	_ = command.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Error("native source plugin did not join", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Error("native source plugin did not stop")
	}
}
